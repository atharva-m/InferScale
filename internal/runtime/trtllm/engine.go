package trtllm

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	platformruntime "github.com/inferscale/inferscale/internal/runtime"
)

// EngineKey covers every input that can change TensorRT engine compatibility.
// It deliberately does not reuse the serving revision digest: runtime image
// identity and GPU architecture are operator-resolved inputs outside the CRD.
func EngineKey(spec platformruntime.Spec) (string, error) {
	if strings.TrimSpace(spec.ModelRevision) == "" || strings.TrimSpace(spec.RuntimeVersion) == "" ||
		strings.TrimSpace(spec.AcceleratorType) == "" || spec.TensorParallel <= 0 || spec.MaxModelLen <= 0 {
		return "", fmt.Errorf("model revision, runtime image, GPU architecture, TP, and context are required for a TensorRT engine key")
	}
	identity := struct {
		SchemaVersion   int    `json:"schema_version"`
		ModelRevision   string `json:"model_revision"`
		RuntimeImage    string `json:"runtime_image"`
		Precision       string `json:"precision"`
		Quantization    string `json:"quantization"`
		TensorParallel  int32  `json:"tensor_parallelism"`
		MaxModelLen     int32  `json:"max_model_len"`
		GPUArchitecture string `json:"gpu_architecture"`
	}{
		SchemaVersion: 1, ModelRevision: strings.ToLower(spec.ModelRevision),
		RuntimeImage: spec.RuntimeVersion, Precision: spec.Precision,
		Quantization: normalizedQuantization(spec.Quantization), TensorParallel: spec.TensorParallel,
		MaxModelLen: spec.MaxModelLen, GPUArchitecture: spec.AcceleratorType,
	}
	payload, err := json.Marshal(identity)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:]), nil
}

func engineCachePath(render platformruntime.RenderContext) (string, string, error) {
	root := filepath.Clean(render.EngineCacheHostPath)
	if !filepath.IsAbs(root) || root == string(filepath.Separator) {
		return "", "", fmt.Errorf("TensorRT engine cache root must be a non-root absolute path")
	}
	key, err := EngineKey(render.Spec)
	if err != nil {
		return "", "", err
	}
	return root, filepath.Join(root, key), nil
}

func runtimeImageDigest(reference string) string {
	index := strings.LastIndex(reference, "@sha256:")
	if index < 0 {
		return ""
	}
	return reference[index+1:]
}

func normalizedQuantization(value string) string {
	if strings.TrimSpace(value) == "" {
		return "none"
	}
	return value
}
