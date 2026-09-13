package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/inferscale/inferscale/internal/deployment"
)

const idempotencyResponseTTL = 24 * time.Hour

type cachedResponse struct {
	RequestDigest string          `json:"request_digest"`
	Status        int             `json:"status"`
	ContentType   string          `json:"content_type"`
	ETag          string          `json:"etag,omitempty"`
	Body          json.RawMessage `json:"body"`
}

func idempotencyCacheKey(scope, key string) string {
	digest := sha256.Sum256([]byte(scope + "\x00" + key))
	return hex.EncodeToString(digest[:])
}

func (s *Server) replayIdempotency(ctx context.Context, w http.ResponseWriter, tenantID, scope, key, requestDigest string) (bool, error) {
	if s.idempotency == nil {
		return false, nil
	}
	raw, found, err := s.idempotency.GetIdempotency(ctx, tenantID, idempotencyCacheKey(scope, key))
	if err != nil {
		s.logger.Warn("read idempotency response cache", "error", err)
		return false, nil
	}
	if !found {
		return false, nil
	}
	var cached cachedResponse
	if json.Unmarshal(raw, &cached) != nil || cached.RequestDigest == "" || cached.Status < 200 || cached.Status > 299 || !json.Valid(cached.Body) {
		s.logger.Warn("ignored invalid idempotency response cache entry")
		return false, nil
	}
	if cached.RequestDigest != requestDigest {
		return false, deployment.ErrIdempotencyConflict
	}
	contentType := strings.TrimSpace(cached.ContentType)
	if contentType == "" {
		contentType = "application/json"
	}
	w.Header().Set("Content-Type", contentType)
	if cached.ETag != "" {
		w.Header().Set("ETag", cached.ETag)
	}
	w.Header().Set("Idempotency-Replayed", "true")
	w.WriteHeader(cached.Status)
	_, _ = w.Write(append(append([]byte(nil), cached.Body...), '\n'))
	return true, nil
}

func (s *Server) writeIdempotentJSON(ctx context.Context, w http.ResponseWriter, tenantID, scope, key, requestDigest string, status int, value any) {
	body, err := json.Marshal(value)
	if err != nil {
		s.logger.Error("marshal idempotent response", "error", err)
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}
	cached := cachedResponse{
		RequestDigest: requestDigest,
		Status:        status, ContentType: "application/json", ETag: w.Header().Get("ETag"), Body: body,
	}
	if s.idempotency != nil {
		encoded, marshalErr := json.Marshal(cached)
		if marshalErr == nil {
			if _, putErr := s.idempotency.PutIdempotency(ctx, tenantID, idempotencyCacheKey(scope, key), encoded, idempotencyResponseTTL); putErr != nil {
				s.logger.Warn("write idempotency response cache", "error", putErr)
			}
		}
	}
	w.Header().Set("Content-Type", cached.ContentType)
	w.WriteHeader(status)
	_, _ = w.Write(append(body, '\n'))
}

// canonicalRequestDigest binds an idempotency key to the accepted logical
// request rather than to JSON whitespace or object-key order. Callers pass a
// typed request with defaults resolved, and canonicalJSON can be used for any
// nested free-form JSON field before hashing it.
func canonicalRequestDigest(value any) (string, error) {
	payload, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("marshal canonical idempotency request: %w", err)
	}
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:]), nil
}

func canonicalJSON(raw json.RawMessage) (json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, errors.New("JSON value must contain one value")
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return canonical, nil
}
