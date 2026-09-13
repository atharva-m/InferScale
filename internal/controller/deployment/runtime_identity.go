package deployment

import (
	"fmt"
	"strings"

	platformruntime "github.com/inferscale/inferscale/internal/runtime"
)

// imagesForResolution replaces the mutable controller configuration for the
// selected backend with the digest that was frozen into the immutable database
// revision. Configuration may change after a controller restart; rendering
// must not silently move an existing revision to that newer image.
func imagesForResolution(images platformruntime.Images, resolution Resolution) (platformruntime.Images, error) {
	var configured *string
	switch resolution.Backend {
	case platformruntime.BackendVLLM:
		configured = &images.VLLM
	case platformruntime.BackendTRTLLM:
		configured = &images.TensorRTLLM
	default:
		return platformruntime.Images{}, fmt.Errorf("cannot resolve image for backend %q", resolution.Backend)
	}
	original := *configured
	frozen, err := frozenImageReference(original, resolution.RuntimeImageDigest)
	if err != nil {
		return platformruntime.Images{}, err
	}
	*configured = frozen
	if resolution.Backend == platformruntime.BackendTRTLLM &&
		RuntimeImageIdentity(images.TensorRTBuild) == RuntimeImageIdentity(original) {
		// The v1 image supplies both trtllm-build and trtllm-serve. Keep the
		// engine builder on the exact same immutable image as the serving pod.
		images.TensorRTBuild = frozen
	}
	return images, nil
}

func frozenImageReference(configured, identity string) (string, error) {
	configured = strings.TrimSpace(configured)
	identity = strings.TrimSpace(identity)
	if configured == "" {
		return "", fmt.Errorf("configured runtime image is empty")
	}
	if identity == "" {
		return "", fmt.Errorf("frozen runtime image identity is empty")
	}
	if strings.HasPrefix(identity, "sha256:") {
		base := configured
		if at := strings.LastIndex(base, "@"); at >= 0 {
			base = base[:at]
		} else if colon := strings.LastIndex(base, ":"); colon > strings.LastIndex(base, "/") {
			base = base[:colon]
		}
		if base == "" {
			return "", fmt.Errorf("cannot combine runtime image %q with frozen digest", configured)
		}
		return base + "@" + identity, nil
	}
	// Development-only configurations can be tag-pinned rather than
	// digest-pinned. RuntimeImageIdentity stores the whole reference in that
	// case, which is still preferable to substituting a newer configured tag.
	if strings.Contains(identity, "/") || strings.Contains(identity, "@") || identity == configured {
		return identity, nil
	}
	return "", fmt.Errorf("unsupported frozen runtime image identity %q", identity)
}
