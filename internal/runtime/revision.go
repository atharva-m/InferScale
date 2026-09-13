package runtime

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	kubeutil "github.com/inferscale/inferscale/internal/kubernetes"
)

// servingContract intentionally excludes scaling, admission, rollout, routing,
// and observability policy. A resolved backend is included so backend:auto is
// frozen for the lifetime of an immutable revision.
type servingContract struct {
	ModelURI          string  `json:"model_uri"`
	ModelRevision     string  `json:"model_revision"`
	Backend           Backend `json:"backend"`
	RuntimeVersion    string  `json:"runtime_version,omitempty"`
	Precision         string  `json:"precision"`
	Quantization      string  `json:"quantization,omitempty"`
	TensorParallel    int32   `json:"tensor_parallelism"`
	MaxModelLen       int32   `json:"max_model_len"`
	PrefixCaching     bool    `json:"prefix_caching"`
	AcceleratorVendor string  `json:"accelerator_vendor"`
	AcceleratorType   string  `json:"accelerator_type"`
	AcceleratorCount  int32   `json:"accelerator_count"`
}

func RevisionFor(spec Spec) (Revision, error) {
	if spec.ResolvedBackend == "" || spec.ResolvedBackend == BackendAuto {
		return Revision{}, fmt.Errorf("resolved backend is required before calculating a revision")
	}
	contract := servingContract{
		ModelURI: spec.ModelURI, ModelRevision: spec.ModelRevision,
		Backend: spec.ResolvedBackend, RuntimeVersion: spec.RuntimeVersion,
		Precision: spec.Precision, Quantization: spec.Quantization,
		TensorParallel: spec.TensorParallel, MaxModelLen: spec.MaxModelLen,
		PrefixCaching:     spec.PrefixCaching,
		AcceleratorVendor: spec.AcceleratorVendor, AcceleratorType: spec.AcceleratorType,
		AcceleratorCount: spec.AcceleratorCount,
	}
	payload, err := json.Marshal(contract)
	if err != nil {
		return Revision{}, fmt.Errorf("marshal serving contract: %w", err)
	}
	sum := sha256.Sum256(payload)
	digest := hex.EncodeToString(sum[:])
	return Revision{Name: kubeutil.ResourceName(spec.DeploymentName, digest[:10]), Digest: digest}, nil
}
