package kubernetes

import "testing"

func TestResourceName(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		parts []string
		want  string
	}{
		{name: "simple", parts: []string{"qwen-chat", "a8f32", "epp"}, want: "qwen-chat-a8f32-epp"},
		{name: "sanitize", parts: []string{"Qwen_Chat", "EPP"}, want: "qwen-chat-epp"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := ResourceName(tt.parts...); got != tt.want {
				t.Fatalf("ResourceName() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestResourceNameTruncatesDeterministically(t *testing.T) {
	t.Parallel()
	part := "this-is-an-extremely-long-deployment-name-that-cannot-fit-in-a-kubernetes-dns-label"
	first := ResourceName(part, "runtime")
	second := ResourceName(part, "runtime")
	if first != second {
		t.Fatalf("names differ: %q != %q", first, second)
	}
	if len(first) > maxDNSLabelLength {
		t.Fatalf("name length = %d, want <= %d", len(first), maxDNSLabelLength)
	}
}
