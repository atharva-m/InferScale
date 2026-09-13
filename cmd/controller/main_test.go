package main

import (
	"testing"

	"github.com/inferscale/inferscale/internal/config"
	platformruntime "github.com/inferscale/inferscale/internal/runtime"
)

func TestRuntimeAdaptersReplaceOnlyVLLMWhenFakeRuntimeIsEnabled(t *testing.T) {
	t.Parallel()

	real, err := platformruntime.NewRegistry(runtimeAdapters(config.Config{})...)
	if err != nil {
		t.Fatal(err)
	}
	realVLLM, err := real.Get(platformruntime.BackendVLLM)
	if err != nil {
		t.Fatal(err)
	}
	if !realVLLM.Capabilities().RequiresModelCache {
		t.Fatal("normal controller must retain the real vLLM cache contract")
	}

	local, err := platformruntime.NewRegistry(runtimeAdapters(config.Config{FakeRuntime: true})...)
	if err != nil {
		t.Fatal(err)
	}
	localVLLM, err := local.Get(platformruntime.BackendVLLM)
	if err != nil {
		t.Fatal(err)
	}
	if localVLLM.Capabilities().RequiresModelCache {
		t.Fatal("local fake vLLM renderer must bypass model prefetch")
	}
	if localVLLM.Name() != platformruntime.BackendVLLM {
		t.Fatalf("fake renderer leaked a public backend %q", localVLLM.Name())
	}
	if _, err := local.Get(platformruntime.BackendTRTLLM); err != nil {
		t.Fatalf("local registry unexpectedly removed TensorRT adapter: %v", err)
	}
}
