package tenant

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apiresource "k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const RuntimeServiceAccount = "inferscale-runtime"

// Kubernetes quota includes one platform-controlled surge copy so an
// immutable candidate (or its TensorRT engine build) can coexist with stable.
// The API continues to enforce Quota.MaxGPUs as the tenant's steady-state
// allocation; tenants cannot submit arbitrary pods or consume this reserve.
const rolloutGPUSurgeFactor = int64(2)

func NamespaceBaseline(value *Tenant) []client.Object {
	return NamespaceBaselineWithOptions(value, NamespaceOptions{})
}

type NamespaceOptions struct {
	// KubernetesAPIServerCIDR narrows the API-server egress rule. When empty,
	// TCP/443 is allowed as a portable fallback because NetworkPolicy cannot
	// select the `default/kubernetes` Service by service identity.
	KubernetesAPIServerCIDR string
}

func NamespaceBaselineWithOptions(value *Tenant, options NamespaceOptions) []client.Object {
	labels := map[string]string{
		"app.kubernetes.io/managed-by":               "inferscale-controller",
		"inferscale.io/tenant":                       value.Slug,
		"inferscale.io/gateway-routes":               "allowed",
		"pod-security.kubernetes.io/enforce":         "privileged",
		"pod-security.kubernetes.io/audit":           "restricted",
		"pod-security.kubernetes.io/warn":            "restricted",
		"pod-security.kubernetes.io/enforce-version": "latest",
	}
	// Server-side apply marshals typed objects directly, so every baseline
	// object must carry the API version and kind required by the API server.
	namespace := &corev1.Namespace{
		TypeMeta:   metav1.TypeMeta{APIVersion: corev1.SchemeGroupVersion.String(), Kind: "Namespace"},
		ObjectMeta: metav1.ObjectMeta{Name: value.Namespace, Labels: labels},
	}
	automountServiceAccountToken := false
	serviceAccount := &corev1.ServiceAccount{
		TypeMeta:                     metav1.TypeMeta{APIVersion: corev1.SchemeGroupVersion.String(), Kind: "ServiceAccount"},
		ObjectMeta:                   metav1.ObjectMeta{Name: RuntimeServiceAccount, Namespace: value.Namespace, Labels: labels},
		AutomountServiceAccountToken: &automountServiceAccountToken,
	}
	quota := &corev1.ResourceQuota{
		TypeMeta: metav1.TypeMeta{APIVersion: corev1.SchemeGroupVersion.String(), Kind: "ResourceQuota"},
		ObjectMeta: metav1.ObjectMeta{
			Name: "inferscale-tenant", Namespace: value.Namespace, Labels: labels,
			Annotations: map[string]string{"inferscale.io/steady-state-gpu-quota": fmt.Sprint(value.Quota.MaxGPUs)},
		},
		Spec: corev1.ResourceQuotaSpec{Hard: corev1.ResourceList{
			corev1.ResourceName("requests.nvidia.com/gpu"): *apiresource.NewQuantity(int64(value.Quota.MaxGPUs)*rolloutGPUSurgeFactor, apiresource.DecimalSI),
			corev1.ResourceName("limits.nvidia.com/gpu"):   *apiresource.NewQuantity(int64(value.Quota.MaxGPUs)*rolloutGPUSurgeFactor, apiresource.DecimalSI),
		}},
	}
	defaultDeny := &networkingv1.NetworkPolicy{
		TypeMeta:   metav1.TypeMeta{APIVersion: networkingv1.SchemeGroupVersion.String(), Kind: "NetworkPolicy"},
		ObjectMeta: metav1.ObjectMeta{Name: "default-deny", Namespace: value.Namespace, Labels: labels},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
		},
	}
	dns := &networkingv1.NetworkPolicy{
		TypeMeta:   metav1.TypeMeta{APIVersion: networkingv1.SchemeGroupVersion.String(), Kind: "NetworkPolicy"},
		ObjectMeta: metav1.ObjectMeta{Name: "allow-dns-egress", Namespace: value.Namespace, Labels: labels},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{}, PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			Egress: []networkingv1.NetworkPolicyEgressRule{{
				To: []networkingv1.NetworkPolicyPeer{{
					NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": "kube-system"}},
					PodSelector:       &metav1.LabelSelector{MatchLabels: map[string]string{"k8s-app": "kube-dns"}},
				}},
				Ports: []networkingv1.NetworkPolicyPort{networkPolicyPort(corev1.ProtocolUDP, 53), networkPolicyPort(corev1.ProtocolTCP, 53)},
			}},
		},
	}
	gateway := &networkingv1.NetworkPolicy{
		TypeMeta:   metav1.TypeMeta{APIVersion: networkingv1.SchemeGroupVersion.String(), Kind: "NetworkPolicy"},
		ObjectMeta: metav1.ObjectMeta{Name: "allow-gateway-ingress", Namespace: value.Namespace, Labels: labels},
		Spec: networkingv1.NetworkPolicySpec{
			// The Gateway connects both to runtime Services (shadow) and the
			// revision EPP (ordinary and canary traffic). The dedicated Gateway
			// namespace is the source trust boundary.
			PodSelector: metav1.LabelSelector{},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			Ingress: []networkingv1.NetworkPolicyIngressRule{{From: []networkingv1.NetworkPolicyPeer{{
				NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"inferscale.io/access-role": "gateway"}},
			}}, Ports: []networkingv1.NetworkPolicyPort{
				networkPolicyPort(corev1.ProtocolTCP, 8000),
				networkPolicyPort(corev1.ProtocolTCP, 9002),
			}}},
		},
	}
	monitoringIngress := &networkingv1.NetworkPolicy{
		TypeMeta:   metav1.TypeMeta{APIVersion: networkingv1.SchemeGroupVersion.String(), Kind: "NetworkPolicy"},
		ObjectMeta: metav1.ObjectMeta{Name: "allow-monitoring-ingress", Namespace: value.Namespace, Labels: labels},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{}, PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			Ingress: []networkingv1.NetworkPolicyIngressRule{{From: []networkingv1.NetworkPolicyPeer{{
				NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"inferscale.io/access-role": "monitoring"}},
				PodSelector:       &metav1.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/name": "prometheus"}},
			}}}},
		},
	}
	otelEgress := &networkingv1.NetworkPolicy{
		TypeMeta:   metav1.TypeMeta{APIVersion: networkingv1.SchemeGroupVersion.String(), Kind: "NetworkPolicy"},
		ObjectMeta: metav1.ObjectMeta{Name: "allow-otel-egress", Namespace: value.Namespace, Labels: labels},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{}, PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			Egress: []networkingv1.NetworkPolicyEgressRule{{
				To: []networkingv1.NetworkPolicyPeer{{
					NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"inferscale.io/access-role": "monitoring"}},
					PodSelector:       &metav1.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/name": "otel-collector"}},
				}},
				Ports: []networkingv1.NetworkPolicyPort{networkPolicyPort(corev1.ProtocolTCP, 4317), networkPolicyPort(corev1.ProtocolTCP, 4318)},
			}},
		},
	}
	sameNamespaceEgress := &networkingv1.NetworkPolicy{
		TypeMeta:   metav1.TypeMeta{APIVersion: networkingv1.SchemeGroupVersion.String(), Kind: "NetworkPolicy"},
		ObjectMeta: metav1.ObjectMeta{Name: "allow-same-namespace-egress", Namespace: value.Namespace, Labels: labels},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{}, PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			Egress: []networkingv1.NetworkPolicyEgressRule{{To: []networkingv1.NetworkPolicyPeer{{PodSelector: &metav1.LabelSelector{}}}}},
		},
	}
	httpsEgress := &networkingv1.NetworkPolicy{
		TypeMeta: metav1.TypeMeta{APIVersion: networkingv1.SchemeGroupVersion.String(), Kind: "NetworkPolicy"},
		ObjectMeta: metav1.ObjectMeta{Name: "allow-https-egress", Namespace: value.Namespace, Labels: labels,
			Annotations: map[string]string{"inferscale.io/purpose": "Hugging Face model fetch and control APIs; restrict by cluster egress policy where available"}},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{}, PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			Egress: []networkingv1.NetworkPolicyEgressRule{{Ports: []networkingv1.NetworkPolicyPort{networkPolicyPort(corev1.ProtocolTCP, 443)}}},
		},
	}
	// An omitted peer list allows TCP/443 to any destination. An explicit
	// empty peer is invalid NetworkPolicy input, rather than that fallback.
	var apiPeers []networkingv1.NetworkPolicyPeer
	if options.KubernetesAPIServerCIDR != "" {
		apiPeers = []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{CIDR: options.KubernetesAPIServerCIDR}}}
	}
	apiEgress := &networkingv1.NetworkPolicy{
		TypeMeta: metav1.TypeMeta{APIVersion: networkingv1.SchemeGroupVersion.String(), Kind: "NetworkPolicy"},
		ObjectMeta: metav1.ObjectMeta{Name: "allow-kubernetes-api-egress", Namespace: value.Namespace, Labels: labels,
			Annotations: map[string]string{"inferscale.io/purpose": "Kubernetes API access for EPP/control components"}},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{}, PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			Egress: []networkingv1.NetworkPolicyEgressRule{{To: apiPeers, Ports: []networkingv1.NetworkPolicyPort{networkPolicyPort(corev1.ProtocolTCP, 443)}}},
		},
	}
	benchmarkCallbackEgress := &networkingv1.NetworkPolicy{
		TypeMeta:   metav1.TypeMeta{APIVersion: networkingv1.SchemeGroupVersion.String(), Kind: "NetworkPolicy"},
		ObjectMeta: metav1.ObjectMeta{Name: "allow-benchmark-callback-egress", Namespace: value.Namespace, Labels: labels},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/name": "inferscale-benchmark"}},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			Egress: []networkingv1.NetworkPolicyEgressRule{{
				To: []networkingv1.NetworkPolicyPeer{{
					NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": "inferscale-system"}},
					PodSelector:       &metav1.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/name": "inferscale-api"}},
				}},
				Ports: []networkingv1.NetworkPolicyPort{networkPolicyPort(corev1.ProtocolTCP, 8080)},
			}},
		},
	}
	benchmarkGatewayEgress := &networkingv1.NetworkPolicy{
		TypeMeta:   metav1.TypeMeta{APIVersion: networkingv1.SchemeGroupVersion.String(), Kind: "NetworkPolicy"},
		ObjectMeta: metav1.ObjectMeta{Name: "allow-benchmark-gateway-egress", Namespace: value.Namespace, Labels: labels},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/name": "inferscale-benchmark"}},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			Egress: []networkingv1.NetworkPolicyEgressRule{{To: []networkingv1.NetworkPolicyPeer{{
				NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"inferscale.io/access-role": "gateway"}},
			}}}},
		},
	}
	benchmarkPrometheusEgress := &networkingv1.NetworkPolicy{
		TypeMeta:   metav1.TypeMeta{APIVersion: networkingv1.SchemeGroupVersion.String(), Kind: "NetworkPolicy"},
		ObjectMeta: metav1.ObjectMeta{Name: "allow-benchmark-prometheus-egress", Namespace: value.Namespace, Labels: labels},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/name": "inferscale-benchmark"}},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			Egress: []networkingv1.NetworkPolicyEgressRule{{
				To: []networkingv1.NetworkPolicyPeer{{
					NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"inferscale.io/access-role": "monitoring"}},
					PodSelector:       &metav1.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/name": "prometheus"}},
				}},
				Ports: []networkingv1.NetworkPolicyPort{networkPolicyPort(corev1.ProtocolTCP, 9090)},
			}},
		},
	}
	return []client.Object{namespace, serviceAccount, quota, defaultDeny, dns, gateway, monitoringIngress, otelEgress, sameNamespaceEgress, httpsEgress, apiEgress, benchmarkCallbackEgress, benchmarkGatewayEgress, benchmarkPrometheusEgress}
}

func networkPolicyPort(protocol corev1.Protocol, port int32) networkingv1.NetworkPolicyPort {
	return networkingv1.NetworkPolicyPort{Protocol: &protocol, Port: &intstr.IntOrString{Type: intstr.Int, IntVal: port}}
}

type NamespaceProvisioner struct {
	Client                  client.Client
	FieldOwner              string
	KubernetesAPIServerCIDR string
}

func (p NamespaceProvisioner) Provision(ctx context.Context, value *Tenant) error {
	if p.Client == nil {
		return fmt.Errorf("tenant namespace client is not configured")
	}
	fieldOwner := p.FieldOwner
	if fieldOwner == "" {
		fieldOwner = "inferscale-controller"
	}
	for _, object := range NamespaceBaselineWithOptions(value, NamespaceOptions{KubernetesAPIServerCIDR: p.KubernetesAPIServerCIDR}) {
		if err := p.Client.Patch(ctx, object, client.Apply, client.FieldOwner(fieldOwner), client.ForceOwnership); err != nil {
			return fmt.Errorf("apply tenant baseline %T %s/%s: %w", object, object.GetNamespace(), object.GetName(), err)
		}
	}
	return nil
}
