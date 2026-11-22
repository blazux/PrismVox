package llm

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLegacyUIBootstrapRetainsAgentCredentialOnlyForSamePrism(t *testing.T) {
	t.Setenv("PRISM_URL", "http://legacy-prism.invalid")
	t.Setenv("PRISM_TOKEN", "agent-only-service-fixture")
	c := New()
	raw := map[string]string{"prism_enabled": "true", "prism_url": "http://legacy-prism.invalid/", "prism_model": ""}
	if err := c.Configure(raw); err != nil {
		t.Fatal(err)
	}
	if c.PrismToken != "agent-only-service-fixture" || !c.Linked() {
		t.Fatal("URL-only UI bootstrap erased the legacy agent credential")
	}
	raw["prism_url"] = "http://different-prism.invalid"
	if err := c.Configure(raw); err != nil {
		t.Fatal(err)
	}
	if c.PrismToken != "" {
		t.Fatal("a different Prism URL inherited the legacy credential")
	}
	raw["prism_url"] = "http://legacy-prism.invalid"
	raw["prism_token"] = ""
	if err := c.Configure(raw); err != nil {
		t.Fatal(err)
	}
	if c.PrismToken != "" {
		t.Fatal("explicitly cleared saved token fell back to the environment")
	}
	if err := c.Configure(nil); err != nil {
		t.Fatal(err)
	}
	if c.PrismToken != "agent-only-service-fixture" || !c.Linked() {
		t.Fatal("a UI without Prism bootstrap disconnected the legacy agent")
	}
}

func TestSavedPrismTokenDecryptsAsConnectionField(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	path := filepath.Join(t.TempDir(), "secret.key")
	if err := os.WriteFile(path, key, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SECRET_KEY_PATH", path)
	block, _ := aes.NewCipher(key)
	gcm, _ := cipher.NewGCM(block)
	nonce := make([]byte, gcm.NonceSize())
	sealed := gcm.Seal(nonce, nonce, []byte("service-fixture"), []byte("prism_token"))
	value := "enc:v1:" + base64.RawURLEncoding.EncodeToString(sealed)
	c := New()
	raw := map[string]string{"prism_enabled": "true", "prism_url": "https://prism.invalid", "prism_token": value, "prism_model": "default"}
	if err := c.Configure(raw); err != nil {
		t.Fatal(err)
	}
	if c.APIKey != "service-fixture" || c.PrismToken != "service-fixture" {
		t.Fatal("encrypted connection token was not read")
	}
	raw["prism_token"] = "enc:v1:invalid-ciphertext"
	if err := c.Configure(raw); err == nil || strings.Contains(err.Error(), "invalid-ciphertext") {
		t.Fatal("invalid credentials were accepted or echoed")
	}
}

func TestVoxTurnsUsePrismInferenceWithoutASecondAgentLoop(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "never-forward-provider-fixture")
	seen := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/prefix/api/voice/inference/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer service-fixture" {
			t.Errorf("unexpected inference request path or credential")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		if body["model"] != "source::fixture-model" {
			t.Errorf("model = %v", body["model"])
		}
		seen++
		if body["stream"] == true {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Fixture response.\"}}]}\n\n")
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"content":"Fixture response."},"finish_reason":"stop"}]}`)
	}))
	defer server.Close()
	c := New()
	if err := c.Configure(map[string]string{"prism_enabled": "true", "prism_url": server.URL + "/prefix",
		"prism_token": "service-fixture", "prism_model": "source::fixture-model"}); err != nil {
		t.Fatal(err)
	}
	if c.Docked() {
		t.Fatal("unknown caller became a personal Prism session")
	}
	text, _, err := c.GetResponse(context.Background(), "Fixture reception", nil, "", "", nil, "Hello")
	if err != nil || text != "Fixture response." {
		t.Fatalf("non-streamed result %q: %v", text, err)
	}
	sentences, tools, errors := c.StreamResponse(context.Background(), "Fixture reception", nil, "", "", nil, "Hello")
	var spoken string
	for sentence := range sentences {
		spoken += sentence
	}
	for range tools {
	}
	if err := <-errors; err != nil {
		t.Fatal(err)
	}
	if spoken != "Fixture response." || seen != 2 {
		t.Fatalf("stream result %q, requests %d", spoken, seen)
	}
}
