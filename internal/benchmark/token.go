package benchmark

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

var ErrInvalidCallbackToken = errors.New("invalid benchmark callback token")

// CallbackTokens issues deterministic, run-scoped bearer tokens. Determinism
// makes Secret and Job creation idempotent across scheduler lease recovery. A
// token has no independent lifetime: the repository's terminal state makes a
// completed or failed run immutable, which revokes further result writes.
type CallbackTokens struct {
	key []byte
}

func NewCallbackTokens(key []byte) (*CallbackTokens, error) {
	if len(key) < 32 {
		return nil, errors.New("benchmark callback signing key must contain at least 32 bytes")
	}
	return &CallbackTokens{key: append([]byte(nil), key...)}, nil
}

func (t *CallbackTokens) Issue(runID string) (string, error) {
	if strings.TrimSpace(runID) == "" {
		return "", errors.New("benchmark run ID is required")
	}
	payload := base64.RawURLEncoding.EncodeToString([]byte(runID))
	signature := t.sign(payload)
	return "v1." + payload + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

// Parse authenticates a run-scoped token and returns the run it belongs to.
// The caller must still load the run and verify that it is active; the token
// deliberately carries no mutable authorization state of its own.
func (t *CallbackTokens) Parse(token string) (string, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] != "v1" {
		return "", ErrInvalidCallbackToken
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || len(payload) == 0 {
		return "", ErrInvalidCallbackToken
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || !hmac.Equal(signature, t.sign(parts[1])) {
		return "", ErrInvalidCallbackToken
	}
	return string(payload), nil
}

func (t *CallbackTokens) Verify(runID, token string) error {
	parsed, err := t.Parse(token)
	if err != nil || subtle.ConstantTimeCompare([]byte(parsed), []byte(runID)) != 1 {
		return ErrInvalidCallbackToken
	}
	return nil
}

func (t *CallbackTokens) sign(payload string) []byte {
	mac := hmac.New(sha256.New, t.key)
	_, _ = fmt.Fprintf(mac, "inferscale-benchmark-callback\x00%s", payload)
	return mac.Sum(nil)
}
