package kubernetes

const (
	LabelManagedBy  = "app.kubernetes.io/managed-by"
	LabelName       = "app.kubernetes.io/name"
	LabelComponent  = "app.kubernetes.io/component"
	LabelDeployment = "inferscale.io/deployment"
	LabelRevision   = "inferscale.io/revision"
	LabelBackend    = "inferscale.io/backend"
	LabelModel      = "inferscale.io/model"
	LabelTenant     = "inferscale.io/tenant"

	AnnotationDeploymentID   = "inferscale.io/deployment-id"
	AnnotationRevisionID     = "inferscale.io/revision-id"
	AnnotationRolloutControl = "inferscale.io/rollout-control"

	ManagedByValue = "inferscale-controller"
)

// Labels returns the canonical identity labels placed on every revision-owned
// resource and worker pod. Values must already be valid Kubernetes label
// values; callers should use ResourceName for user-derived values.
func Labels(deployment, revision, backend, model, tenant string) map[string]string {
	labels := map[string]string{
		LabelManagedBy:  ManagedByValue,
		LabelName:       "inferscale-runtime",
		LabelDeployment: deployment,
		LabelRevision:   revision,
		LabelBackend:    backend,
	}
	if model != "" {
		labels[LabelModel] = model
	}
	if tenant != "" {
		labels[LabelTenant] = tenant
	}
	return labels
}

func CopyLabels(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}
