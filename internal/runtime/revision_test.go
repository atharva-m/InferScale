package runtime

import "testing"

func TestRevisionExcludesPolicyFields(t *testing.T) {
	t.Parallel()
	base := Spec{
		DeploymentName: "chat", Namespace: "tenant-a", ModelURI: "hf://Qwen/Qwen3-8B",
		ModelRevision: "abc123", ResolvedBackend: BackendVLLM, Precision: "bf16",
		TensorParallel: 1, MaxModelLen: 8192, AcceleratorVendor: "nvidia",
		AcceleratorType: "RTX_5090", AcceleratorCount: 1, MinReplicas: 1, MaxReplicas: 8,
	}
	first, err := RevisionFor(base)
	if err != nil {
		t.Fatal(err)
	}
	base.MinReplicas = 0
	base.MaxQueuedRequests = 200
	base.Tracing = true
	second, err := RevisionFor(base)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("policy-only update changed revision: %#v != %#v", first, second)
	}
	base.PrefixCaching = true
	third, err := RevisionFor(base)
	if err != nil {
		t.Fatal(err)
	}
	if first == third {
		t.Fatal("serving-contract update did not change revision")
	}
}
