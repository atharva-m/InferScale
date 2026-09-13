package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	yaml "go.yaml.in/yaml/v3"
)

type lockFile struct {
	Components  map[string]componentLock `yaml:"components"`
	Conformance struct {
		Status           string   `yaml:"status"`
		RequiredSuite    string   `yaml:"requiredSuite"`
		RequiredFeatures []string `yaml:"requiredFeatures"`
	} `yaml:"conformance"`
	Policy struct {
		BlockReleaseWhileConformancePending bool `yaml:"blockReleaseWhileConformancePending"`
	} `yaml:"policy"`
}

type componentLock struct {
	Version               string `yaml:"version"`
	ImageDigest           string `yaml:"imageDigest"`
	EndpointPickerVersion string `yaml:"endpointPickerVersion"`
	EndpointPickerDigest  string `yaml:"endpointPickerDigest"`
}

type evidenceFile struct {
	SchemaVersion      int                        `json:"schemaVersion"`
	RecordedAt         time.Time                  `json:"recordedAt"`
	VersionsLockSHA256 string                     `json:"versionsLockSHA256"`
	Matrix             map[string]string          `json:"matrix"`
	Features           map[string]featureEvidence `json:"features"`
}

type featureEvidence struct {
	Passed   bool   `json:"passed"`
	Evidence string `json:"evidence"`
}

func main() {
	lockPath := flag.String("lock", "versions.lock.yaml", "pinned integration lock")
	evidencePath := flag.String("evidence", "tests/conformance/gateway/evidence.json", "live conformance evidence")
	flag.Parse()
	if err := verify(*lockPath, *evidencePath, time.Now().UTC()); err != nil {
		fmt.Fprintln(os.Stderr, "release gate blocked:", err)
		os.Exit(1)
	}
	fmt.Println("live Gateway integration evidence matches the pinned release matrix")
}

func verify(lockPath, evidencePath string, now time.Time) error {
	lockBytes, err := os.ReadFile(lockPath)
	if err != nil {
		return err
	}
	var lock lockFile
	if err := yaml.Unmarshal(lockBytes, &lock); err != nil {
		return fmt.Errorf("parse %s: %w", lockPath, err)
	}
	if lock.Policy.BlockReleaseWhileConformancePending && lock.Conformance.Status != "verified" {
		return fmt.Errorf("conformance status is %q, want verified", lock.Conformance.Status)
	}
	if lock.Conformance.RequiredSuite != "tests/conformance/gateway" {
		return fmt.Errorf("requiredSuite %q is not the checked-in harness", lock.Conformance.RequiredSuite)
	}
	evidenceBytes, err := os.ReadFile(evidencePath)
	if err != nil {
		return fmt.Errorf("read live evidence: %w", err)
	}
	var evidence evidenceFile
	decoder := json.NewDecoder(strings.NewReader(string(evidenceBytes)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&evidence); err != nil {
		return fmt.Errorf("parse live evidence: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return fmt.Errorf("parse live evidence: expected exactly one JSON object")
	}
	if evidence.SchemaVersion != 1 {
		return fmt.Errorf("evidence schemaVersion=%d, want 1", evidence.SchemaVersion)
	}
	if evidence.RecordedAt.IsZero() || evidence.RecordedAt.After(now.Add(5*time.Minute)) {
		return fmt.Errorf("evidence recordedAt is missing or in the future")
	}
	if now.Sub(evidence.RecordedAt) > 30*24*time.Hour {
		return fmt.Errorf("live evidence is older than 30 days")
	}
	digest := sha256.Sum256(lockBytes)
	wantDigest := "sha256:" + hex.EncodeToString(digest[:])
	if evidence.VersionsLockSHA256 != wantDigest {
		return fmt.Errorf("evidence lock digest=%q, want %q", evidence.VersionsLockSHA256, wantDigest)
	}
	for _, component := range []string{
		"envoyGateway", "gatewayAPI", "gatewayAPIInferenceExtension", "llmd", "keda", "vllm", "tensorrtLLM",
	} {
		locked, exists := lock.Components[component]
		if !exists || locked.Version == "" {
			return fmt.Errorf("versions lock omits required matrix component %s", component)
		}
		expected := locked.Version
		if component == "llmd" {
			if locked.EndpointPickerVersion == "" || locked.EndpointPickerDigest == "" {
				return fmt.Errorf("versions lock has incomplete llm-d endpoint-picker identity")
			}
			expected += "/epp:" + locked.EndpointPickerVersion + "@" + locked.EndpointPickerDigest
		} else if locked.ImageDigest != "" {
			expected += "@" + locked.ImageDigest
		}
		if evidence.Matrix[component] != expected {
			return fmt.Errorf("evidence matrix %s=%q, want %q", component, evidence.Matrix[component], expected)
		}
	}
	missing := make([]string, 0)
	for _, feature := range lock.Conformance.RequiredFeatures {
		result, ok := evidence.Features[feature]
		if !ok || !result.Passed || strings.TrimSpace(result.Evidence) == "" {
			missing = append(missing, feature)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return fmt.Errorf("features lack passing live evidence: %s", strings.Join(missing, ", "))
	}
	return nil
}
