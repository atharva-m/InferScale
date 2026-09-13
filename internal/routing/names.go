package routing

import (
	kubeutil "github.com/inferscale/inferscale/internal/kubernetes"
	platformruntime "github.com/inferscale/inferscale/internal/runtime"
)

func Names(revision platformruntime.Revision) RevisionNames {
	return RevisionNames{
		Pool:              kubeutil.ResourceName(revision.Name, "pool"),
		EndpointPicker:    kubeutil.ResourceName(revision.Name, "epp"),
		EndpointPickerSvc: kubeutil.ResourceName(revision.Name, "epp"),
		ConfigMap:         kubeutil.ResourceName(revision.Name, "epp-config"),
		ServiceAccount:    kubeutil.ResourceName(revision.Name, "epp"),
		Interactive:       kubeutil.ResourceName(revision.Name, "interactive"),
		Standard:          kubeutil.ResourceName(revision.Name, "standard"),
		Batch:             kubeutil.ResourceName(revision.Name, "batch"),
	}
}
