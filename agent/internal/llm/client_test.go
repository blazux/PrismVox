package llm

import "testing"

// Regression: Prism and Vox must agree on what the gateway variable holds.
// OPENAI_BASE_URL is the /v1 root (used verbatim); the deprecated SGLANG_URL
// meant the host root and gets /v1 appended. Getting this wrong produced a
// silent /v1/v1/chat/completions.
func TestOpenAIBaseURLConvention(t *testing.T) {
	for _, tc := range []struct{ name, base, sglang, want string }{
		{"new var is the /v1 root", "http://gw:4000/v1", "", "http://gw:4000/v1"},
		{"trailing slash trimmed", "http://gw:4000/v1/", "", "http://gw:4000/v1"},
		{"new var wins over legacy", "http://gw:4000/v1", "http://old:1/", "http://gw:4000/v1"},
		{"legacy gets /v1 appended", "", "http://gw:4000", "http://gw:4000/v1"},
		{"legacy trailing slash", "", "http://gw:4000/", "http://gw:4000/v1"},
		{"neither set", "", "", "http://llm:30000/v1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("OPENAI_BASE_URL", tc.base)
			t.Setenv("SGLANG_URL", tc.sglang)
			if got := openAIBaseURL(); got != tc.want {
				t.Fatalf("openAIBaseURL() = %q, want %q", got, tc.want)
			}
		})
	}
}
