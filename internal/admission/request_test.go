package admission

import (
	"strings"
	"testing"

	authv3 "github.com/envoyproxy/go-control-plane/envoy/service/auth/v3"
	"google.golang.org/protobuf/proto"
)

func TestValidateInferenceRequestSubset(t *testing.T) {
	valid := []byte(`{"model":"chat","messages":[{"role":"system","content":"Be concise"},{"role":"user","content":"Hi"}],"stream":true,"stream_options":{"include_usage":true},"max_tokens":64,"temperature":0.2,"top_p":0.9,"stop":["END"],"seed":7}`)
	if err := ValidateInferenceRequest(valid, "chat"); err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string][]byte{
		"wrong model": []byte(`{"model":"other","messages":[{"role":"user","content":"Hi"}]}`),
		"tool call":   []byte(`{"model":"chat","messages":[{"role":"user","content":"Hi"}],"tools":[]}`),
		"image part":  []byte(`{"model":"chat","messages":[{"role":"user","content":[{"type":"image_url"}]}]}`),
	} {
		t.Run(name, func(t *testing.T) {
			if err := ValidateInferenceRequest(value, "chat"); err == nil {
				t.Fatal("unsupported request was accepted")
			}
		})
	}
}

func TestExtAuthEnvelopeAccommodatesBodyAtLimit(t *testing.T) {
	t.Parallel()
	prefix := `{"model":"chat","messages":[{"role":"user","content":"`
	suffix := `"}]}`
	body := []byte(prefix + strings.Repeat("x", MaxInferenceRequestBytes-len(prefix)-len(suffix)) + suffix)
	if len(body) != MaxInferenceRequestBytes {
		t.Fatalf("body size = %d, want %d", len(body), MaxInferenceRequestBytes)
	}
	if err := ValidateInferenceRequest(body, "chat"); err != nil {
		t.Fatalf("body at documented limit was rejected: %v", err)
	}
	request := &authv3.CheckRequest{Attributes: &authv3.AttributeContext{
		Request: &authv3.AttributeContext_Request{Http: &authv3.AttributeContext_HttpRequest{
			Path:    "/v1/deployments/018f6d72-6d22-7b31-a2ac-6a90739016e5/chat/completions",
			Headers: map[string]string{"authorization": "Bearer test", "content-type": "application/json", "x-request-id": strings.Repeat("r", 128)},
			RawBody: body,
		}},
	}}
	serializedSize := proto.Size(request)
	if serializedSize <= MaxInferenceRequestBytes {
		t.Fatalf("test envelope size = %d did not exercise protobuf overhead", serializedSize)
	}
	if serializedSize > MaxExtAuthGRPCMessageBytes {
		t.Fatalf("valid ext-auth envelope size = %d exceeds receive limit %d", serializedSize, MaxExtAuthGRPCMessageBytes)
	}
	if MaxInferenceRequestBytes != 1<<20 {
		t.Fatalf("Envoy/application body cap changed to %d", MaxInferenceRequestBytes)
	}
}
