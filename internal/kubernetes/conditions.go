package kubernetes

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SetCondition upserts a condition while preserving LastTransitionTime when
// its semantic state did not change.
func SetCondition(conditions *[]metav1.Condition, condition metav1.Condition, now metav1.Time) {
	condition.LastTransitionTime = now
	for i := range *conditions {
		current := &(*conditions)[i]
		if current.Type != condition.Type {
			continue
		}
		if current.Status == condition.Status && current.Reason == condition.Reason && current.Message == condition.Message {
			condition.LastTransitionTime = current.LastTransitionTime
		}
		(*conditions)[i] = condition
		return
	}
	*conditions = append(*conditions, condition)
}

func FindCondition(conditions []metav1.Condition, conditionType string) *metav1.Condition {
	for i := range conditions {
		if conditions[i].Type == conditionType {
			condition := conditions[i]
			return &condition
		}
	}
	return nil
}
