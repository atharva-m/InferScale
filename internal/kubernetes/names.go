package kubernetes

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

const maxDNSLabelLength = 63

// ResourceName returns a deterministic DNS-label-safe child-resource name.
// A digest suffix is retained when truncation is required, preventing two long
// deployment names with the same prefix from colliding.
func ResourceName(parts ...string) string {
	raw := strings.Join(parts, "-")
	name := sanitizeDNSLabel(raw)
	if len(name) <= maxDNSLabelLength {
		return name
	}

	sum := sha256.Sum256([]byte(raw))
	suffix := hex.EncodeToString(sum[:])[:8]
	prefixLen := maxDNSLabelLength - len(suffix) - 1
	prefix := strings.Trim(name[:prefixLen], "-")
	return prefix + "-" + suffix
}

func sanitizeDNSLabel(value string) string {
	value = strings.ToLower(value)
	var b strings.Builder
	b.Grow(len(value))
	previousDash := false
	for _, r := range value {
		valid := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		if valid {
			b.WriteRune(r)
			previousDash = false
			continue
		}
		if !previousDash && b.Len() > 0 {
			b.WriteByte('-')
			previousDash = true
		}
	}
	return strings.Trim(b.String(), "-")
}

// ShortDigest returns a stable, human-readable revision suffix.
func ShortDigest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])[:10]
}
