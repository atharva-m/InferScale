package admission

import (
	"strconv"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
)

const (
	HeaderFairnessID    = "x-llm-d-inference-fairness-id"
	HeaderObjective     = "x-llm-d-inference-objective"
	HeaderModelRewrite  = "x-llm-d-model-name-rewrite"
	HeaderTTFTSLO       = "x-llm-d-slo-ttft-ms"
	HeaderTPOTSLO       = "x-llm-d-slo-tpot-ms"
	HeaderTenantID      = "x-inferscale-tenant-id"
	HeaderDeploymentID  = "x-inferscale-deployment-id"
	HeaderPriorityClass = "x-inferscale-priority-class"
)

var untrustedHeaders = []string{
	"authorization",
	HeaderFairnessID,
	HeaderObjective,
	HeaderModelRewrite,
	HeaderTTFTSLO,
	HeaderTPOTSLO,
	HeaderTenantID,
	HeaderDeploymentID,
	HeaderPriorityClass,
	"x-gateway-inference-fairness-id",
	"x-gateway-inference-objective",
	"x-gateway-model-name-rewrite",
}

func StripUntrusted() []string {
	return append([]string(nil), untrustedHeaders...)
}

// StripUntrustedScheduling preserves the bearer credential for routes whose
// own service middleware performs authentication, while preventing callers
// from smuggling trusted inference scheduling metadata through the Gateway.
func StripUntrustedScheduling() []string {
	return append([]string(nil), untrustedHeaders[1:]...)
}

func InjectTrusted(principal Principal, policy DeploymentPolicy) []*corev3.HeaderValueOption {
	values := map[string]string{
		HeaderFairnessID:    principal.TenantID,
		HeaderModelRewrite:  policy.Name,
		HeaderTenantID:      principal.TenantID,
		HeaderDeploymentID:  policy.ID,
		HeaderPriorityClass: policy.PriorityClass,
	}
	if policy.TTFTTargetMS > 0 {
		values[HeaderTTFTSLO] = strconv.FormatInt(policy.TTFTTargetMS, 10)
	}
	if policy.TPOTTargetMS > 0 {
		values[HeaderTPOTSLO] = strconv.FormatInt(policy.TPOTTargetMS, 10)
	}

	headers := make([]*corev3.HeaderValueOption, 0, len(values))
	for key, value := range values {
		headers = append(headers, &corev3.HeaderValueOption{
			Header:       &corev3.HeaderValue{Key: key, Value: value},
			AppendAction: corev3.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD,
		})
	}
	return headers
}
