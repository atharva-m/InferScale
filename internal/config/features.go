package config

import "fmt"

type Features struct {
	ScaleToZero        bool
	ProgressiveRollout bool
	TensorRTLLM        bool
	BackendAuto        bool
}

func LoadFeatures() (Features, error) {
	features := Features{}
	values := []struct {
		name   string
		target *bool
	}{
		{"INFERSCALE_FEATURE_SCALE_TO_ZERO", &features.ScaleToZero},
		{"INFERSCALE_FEATURE_PROGRESSIVE_ROLLOUT", &features.ProgressiveRollout},
		{"INFERSCALE_FEATURE_TRTLLM", &features.TensorRTLLM},
		{"INFERSCALE_FEATURE_BACKEND_AUTO", &features.BackendAuto},
	}
	for _, item := range values {
		value, err := envBool(item.name, false)
		if err != nil {
			return Features{}, fmt.Errorf("load feature gates: %w", err)
		}
		*item.target = value
	}
	return features, nil
}
