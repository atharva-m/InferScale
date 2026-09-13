package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestVerifyRejectsPendingMatrixBeforeReadingEvidence(t *testing.T) {
	root := t.TempDir()
	lockPath := filepath.Join(root, "versions.lock.yaml")
	if err := os.WriteFile(lockPath, []byte(testLock("pending")), 0o600); err != nil {
		t.Fatal(err)
	}
	err := verify(lockPath, filepath.Join(root, "missing.json"), time.Now().UTC())
	if err == nil || !strings.Contains(err.Error(), "want verified") {
		t.Fatalf("pending matrix error=%v", err)
	}
}

func TestVerifyAcceptsFreshCompleteEvidenceForExactLock(t *testing.T) {
	root := t.TempDir()
	lockBytes := []byte(testLock("verified"))
	lockPath := filepath.Join(root, "versions.lock.yaml")
	if err := os.WriteFile(lockPath, lockBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(lockBytes)
	evidence := evidenceFile{
		SchemaVersion:      1,
		RecordedAt:         time.Now().UTC().Add(-time.Hour),
		VersionsLockSHA256: "sha256:" + hex.EncodeToString(digest[:]),
		Matrix: map[string]string{
			"envoyGateway":                 "v1@sha256:test",
			"gatewayAPI":                   "v1",
			"gatewayAPIInferenceExtension": "v1",
			"llmd":                         "v1/epp:v1@sha256:test",
			"keda":                         "v1@sha256:test",
			"vllm":                         "v1@sha256:test",
			"tensorrtLLM":                  "v1@sha256:test",
		},
		Features: map[string]featureEvidence{
			"external-authorization": {Passed: true, Evidence: "artifact://ext-auth"},
			"url-rewrite":            {Passed: true, Evidence: "artifact://rewrite"},
		},
	}
	evidenceBytes, err := json.Marshal(evidence)
	if err != nil {
		t.Fatal(err)
	}
	evidencePath := filepath.Join(root, "evidence.json")
	if err := os.WriteFile(evidencePath, evidenceBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := verify(lockPath, evidencePath, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(evidencePath, append(evidenceBytes, []byte(`{"extra":true}`)...), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := verify(lockPath, evidencePath, time.Now().UTC()); err == nil || !strings.Contains(err.Error(), "exactly one JSON object") {
		t.Fatalf("trailing evidence error=%v", err)
	}

	evidence.Features["url-rewrite"] = featureEvidence{Passed: false, Evidence: "artifact://rewrite"}
	evidenceBytes, _ = json.Marshal(evidence)
	if err := os.WriteFile(evidencePath, evidenceBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := verify(lockPath, evidencePath, time.Now().UTC()); err == nil || !strings.Contains(err.Error(), "url-rewrite") {
		t.Fatalf("failed feature error=%v", err)
	}
}

func testLock(status string) string {
	return `components:
  envoyGateway: {version: v1, imageDigest: "sha256:test"}
  gatewayAPI: {version: v1}
  gatewayAPIInferenceExtension: {version: v1}
  llmd: {version: v1, endpointPickerVersion: v1, endpointPickerDigest: "sha256:test"}
  keda: {version: v1, imageDigest: "sha256:test"}
  vllm: {version: v1, imageDigest: "sha256:test"}
  tensorrtLLM: {version: v1, imageDigest: "sha256:test"}
conformance:
  status: ` + status + `
  requiredSuite: tests/conformance/gateway
  requiredFeatures:
    - external-authorization
    - url-rewrite
policy:
  blockReleaseWhileConformancePending: true
`
}
