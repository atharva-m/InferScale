package benchmark

import (
	"errors"
	"testing"
)

func TestCallbackTokensAreScopedAndTamperEvident(t *testing.T) {
	tokens, err := NewCallbackTokens([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	token, err := tokens.Issue("run-a")
	if err != nil {
		t.Fatal(err)
	}
	if err := tokens.Verify("run-a", token); err != nil {
		t.Fatalf("verify issued token: %v", err)
	}
	if runID, err := tokens.Parse(token); err != nil || runID != "run-a" {
		t.Fatalf("Parse() = %q, %v", runID, err)
	}
	if err := tokens.Verify("run-b", token); !errors.Is(err, ErrInvalidCallbackToken) {
		t.Fatalf("cross-run token error = %v", err)
	}
	if err := tokens.Verify("run-a", token+"x"); !errors.Is(err, ErrInvalidCallbackToken) {
		t.Fatalf("tampered token error = %v", err)
	}
}

func TestCallbackTokensRequireStrongKey(t *testing.T) {
	if _, err := NewCallbackTokens([]byte("short")); err == nil {
		t.Fatal("expected short callback key to fail")
	}
}
