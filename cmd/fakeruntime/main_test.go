package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestChatCompletionsNonStreamingAndStreaming(t *testing.T) {
	application := &server{model: "chat"}
	for _, test := range []struct {
		name, body, contentType, contains string
	}{
		{"json", `{"model":"chat","messages":[{"role":"user","content":"hello"}],"max_tokens":8,"temperature":0,"top_p":1,"stop":["END"],"seed":7}`, "application/json", `"object":"chat.completion"`},
		{"sse", `{"model":"chat","messages":[{"role":"user","content":"hello"}],"stream":true,"stream_options":{"include_usage":true}}`, "text/event-stream", "data: [DONE]"},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(test.body))
			recorder := httptest.NewRecorder()
			application.chatCompletions(recorder, request)
			if recorder.Code != http.StatusOK || !strings.HasPrefix(recorder.Header().Get("Content-Type"), test.contentType) {
				t.Fatalf("status=%d content-type=%q body=%s", recorder.Code, recorder.Header().Get("Content-Type"), recorder.Body.String())
			}
			if !strings.Contains(recorder.Body.String(), test.contains) {
				t.Fatalf("body=%s", recorder.Body.String())
			}
		})
	}
}

func TestChatCompletionsRejectsOversizedAndRuntimeExtensions(t *testing.T) {
	application := &server{model: "chat"}
	for _, body := range [][]byte{
		bytes.Repeat([]byte("x"), maxRequestBytes+1),
		[]byte(`{"model":"chat","messages":[{"role":"user","content":"hello"}],"tools":[]}`),
		[]byte(`{"model":"chat","messages":[{"role":"user","content":"hello"}],"stream_options":{"include_usage":true}}`),
		[]byte(`{"model":"chat","messages":[{"role":"user","content":"hello"}],"stop":[]}`),
	} {
		request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
		recorder := httptest.NewRecorder()
		application.chatCompletions(recorder, request)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
		}
	}
}
