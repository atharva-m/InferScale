package admission

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

const (
	// MaxInferenceRequestBytes is the Envoy buffered-body policy and the
	// application-level inference request limit. It intentionally remains 1 MiB.
	MaxInferenceRequestBytes = 1 << 20
	// MaxExtAuthGRPCMessageBytes includes protobuf framing, request attributes,
	// and HTTP headers around a body at the limit. Keeping this distinct avoids
	// gRPC rejecting a valid 1 MiB request before Check can enforce the body cap.
	MaxExtAuthGRPCMessageBytes = 2 << 20
)

type inferenceRequest struct {
	Model         string                 `json:"model"`
	Messages      []inferenceMessage     `json:"messages"`
	Stream        bool                   `json:"stream,omitempty"`
	StreamOptions *inferenceStreamOption `json:"stream_options,omitempty"`
	MaxTokens     *int64                 `json:"max_tokens,omitempty"`
	Temperature   *float64               `json:"temperature,omitempty"`
	TopP          *float64               `json:"top_p,omitempty"`
	Stop          json.RawMessage        `json:"stop,omitempty"`
	Seed          *int64                 `json:"seed,omitempty"`
}

type inferenceMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type inferenceStreamOption struct {
	IncludeUsage bool `json:"include_usage"`
}

// ValidateInferenceRequest enforces the deliberately small, text-only OpenAI
// request surface before a body reaches a runtime. It never logs or retains the
// body. Envoy buffers at most MaxInferenceRequestBytes for this ext-auth check.
func ValidateInferenceRequest(body []byte, expectedModel string) error {
	if len(body) == 0 {
		return errors.New("request body is required")
	}
	if len(body) > MaxInferenceRequestBytes {
		return errors.New("request body exceeds 1 MiB")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var request inferenceRequest
	if err := decoder.Decode(&request); err != nil {
		return fmt.Errorf("invalid text chat request: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("request body must contain exactly one JSON object")
	}
	if request.Model == "" || request.Model != expectedModel {
		return errors.New("model must equal the deployment name")
	}
	if len(request.Messages) == 0 {
		return errors.New("at least one text message is required")
	}
	for index, message := range request.Messages {
		switch message.Role {
		case "system", "user", "assistant":
		default:
			return fmt.Errorf("messages[%d].role must be system, user, or assistant", index)
		}
		if strings.TrimSpace(message.Content) == "" {
			return fmt.Errorf("messages[%d].content must be a non-empty string", index)
		}
	}
	if request.MaxTokens != nil && *request.MaxTokens < 1 {
		return errors.New("max_tokens must be positive")
	}
	if request.Temperature != nil && (*request.Temperature < 0 || *request.Temperature > 2) {
		return errors.New("temperature must be between 0 and 2")
	}
	if request.TopP != nil && (*request.TopP <= 0 || *request.TopP > 1) {
		return errors.New("top_p must be greater than 0 and at most 1")
	}
	if request.StreamOptions != nil && !request.Stream {
		return errors.New("stream_options requires stream=true")
	}
	if len(request.Stop) > 0 && string(request.Stop) != "null" {
		if err := validateStop(request.Stop); err != nil {
			return err
		}
	}
	return nil
}

func validateStop(raw json.RawMessage) error {
	var single string
	if json.Unmarshal(raw, &single) == nil {
		if single == "" {
			return errors.New("stop string cannot be empty")
		}
		return nil
	}
	var many []string
	if err := json.Unmarshal(raw, &many); err != nil || len(many) == 0 || len(many) > 4 {
		return errors.New("stop must be a string or an array of one to four strings")
	}
	for _, value := range many {
		if value == "" {
			return errors.New("stop strings cannot be empty")
		}
	}
	return nil
}
