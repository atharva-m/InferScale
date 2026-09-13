package runtime

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// WorkerTerminationGracePeriodSeconds bounds the complete Kubernetes
	// shutdown sequence, including the endpoint propagation delay and the
	// runtime's native SIGTERM handling.
	WorkerTerminationGracePeriodSeconds int64 = 120
	// WorkerEndpointPropagationDelaySeconds gives EndpointSlice consumers time
	// to observe ready=false/terminating=true before the native runtime receives
	// SIGTERM. Existing streams stay connected while new requests are routed to
	// another ready endpoint.
	WorkerEndpointPropagationDelaySeconds int64 = 15
)

func RuntimeService(namespace, name string, labels map[string]string) *corev1.Service {
	return &corev1.Service{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Service"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: labels},
		Spec: corev1.ServiceSpec{
			Selector: labels,
			Ports:    []corev1.ServicePort{{Name: "http", Port: ServingPort, Protocol: corev1.ProtocolTCP}},
		},
	}
}

func PullSecrets(name string) []corev1.LocalObjectReference {
	if name == "" {
		return nil
	}
	return []corev1.LocalObjectReference{{Name: name}}
}

// WorkerDrainLifecycle delays SIGTERM with Kubernetes' native sleep lifecycle
// action. Pod deletion itself makes the EndpointSlice endpoint terminating and
// not ready; this pause lets kube-proxy and the endpoint picker observe that
// state before the runtime begins its own graceful shutdown. The sleep action
// runs in the kubelet, so runtime images need no shell, hook binary, or proxy.
func WorkerDrainLifecycle() *corev1.Lifecycle {
	return &corev1.Lifecycle{PreStop: &corev1.LifecycleHandler{
		Sleep: &corev1.SleepAction{Seconds: WorkerEndpointPropagationDelaySeconds},
	}}
}

func Int64Pointer(value int64) *int64 { return &value }

func HostPathTypePointer(value corev1.HostPathType) *corev1.HostPathType { return &value }
