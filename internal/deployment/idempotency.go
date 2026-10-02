package deployment

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// Fingerprints deliberately omit request IDs and idempotency keys: a network
// retry can receive a new request ID while identifying the same logical work.
// Hashing the desired spec binds operational policy as well as serving fields.
func createRequestDigest(input CreateInput) string {
	return operationRequestDigest(OperationCreate, struct {
		TenantID, Namespace, Name, Spec string
	}{input.TenantID, input.Namespace, input.Name, SpecDigest(input.Spec)})
}

func updateRequestDigest(input UpdateInput) string {
	return operationRequestDigest(OperationUpdate, struct {
		TenantID, DeploymentID, Spec string
		ExpectedGeneration           int64
		ForceReselect                bool
	}{input.TenantID, input.DeploymentID, SpecDigest(input.Spec), input.ExpectedGeneration, input.ForceReselect})
}

func deleteRequestDigest(input DeleteInput) string {
	return operationRequestDigest(OperationDelete, struct {
		TenantID, DeploymentID string
		ExpectedGeneration     int64
	}{input.TenantID, input.DeploymentID, input.ExpectedGeneration})
}

func operationRequestDigest(kind OperationKind, request any) string {
	// These private callers supply only strings, booleans, and integers, so
	// JSON marshaling cannot fail. Include a domain/version for future changes.
	payload, _ := json.Marshal(struct {
		Contract string
		Kind     OperationKind
		Request  any
	}{"inferscale:deployment-operation:v1", kind, request})
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:])
}
