package trtllm

import (
	"strings"
	"testing"

	platformruntime "github.com/inferscale/inferscale/internal/runtime"
)

func TestEngineKeyCoversRuntimeAndGPUCompatibility(t *testing.T) {
	base := platformruntime.Spec{
		ModelRevision:  strings.Repeat("a", 40),
		RuntimeVersion: "trtllm:1.0.0@sha256:" + strings.Repeat("b", 64),
		Precision:      "bf16", Quantization: "none", TensorParallel: 2,
		MaxModelLen: 8192, AcceleratorType: "RTX_5090",
	}
	first, err := EngineKey(base)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 64 {
		t.Fatalf("key = %q", first)
	}
	changed := base
	changed.RuntimeVersion = "trtllm:1.0.1@sha256:" + strings.Repeat("c", 64)
	second, _ := EngineKey(changed)
	changed = base
	changed.AcceleratorType = "L40S"
	third, _ := EngineKey(changed)
	if first == second || first == third || second == third {
		t.Fatalf("engine compatibility keys collided: %s %s %s", first, second, third)
	}
}
