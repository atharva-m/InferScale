package deployment

import (
	"testing"

	platformruntime "github.com/inferscale/inferscale/internal/runtime"
)

func TestImagesForResolutionUsesFrozenDigestInsteadOfCurrentConfig(t *testing.T) {
	t.Parallel()
	images, err := imagesForResolution(platformruntime.Images{
		VLLM: "ghcr.io/inferscale/vllm:new",
	}, Resolution{
		Backend: platformruntime.BackendVLLM, RuntimeImageDigest: "sha256:frozen",
	})
	if err != nil {
		t.Fatal(err)
	}
	if images.VLLM != "ghcr.io/inferscale/vllm@sha256:frozen" {
		t.Fatalf("vLLM image=%q", images.VLLM)
	}
}

func TestImagesForResolutionFreezesTensorRTBuilderAndServer(t *testing.T) {
	t.Parallel()
	images, err := imagesForResolution(platformruntime.Images{
		TensorRTLLM:   "registry.example:5000/runtime/trtllm:new",
		TensorRTBuild: "registry.example:5000/runtime/trtllm:new",
	}, Resolution{
		Backend: platformruntime.BackendTRTLLM, RuntimeImageDigest: "sha256:frozen",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "registry.example:5000/runtime/trtllm@sha256:frozen"
	if images.TensorRTLLM != want || images.TensorRTBuild != want {
		t.Fatalf("server=%q builder=%q, want %q", images.TensorRTLLM, images.TensorRTBuild, want)
	}
}

func TestImagesForResolutionRejectsMissingFrozenIdentity(t *testing.T) {
	t.Parallel()
	_, err := imagesForResolution(
		platformruntime.Images{VLLM: "ghcr.io/inferscale/vllm:new"},
		Resolution{Backend: platformruntime.BackendVLLM},
	)
	if err == nil {
		t.Fatal("missing frozen identity must not fall back to current configuration")
	}
}

func TestFrozenImageReferenceRetainsPreviouslyFrozenDevelopmentTag(t *testing.T) {
	t.Parallel()
	got, err := frozenImageReference("ghcr.io/inferscale/vllm:new", "ghcr.io/inferscale/vllm:old")
	if err != nil {
		t.Fatal(err)
	}
	if got != "ghcr.io/inferscale/vllm:old" {
		t.Fatalf("image=%q", got)
	}
}
