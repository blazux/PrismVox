package llm

import "testing"

func TestUnifiedModelIgnoresRetiredOncallAndPreservesCredentials(t *testing.T) {
	t.Setenv("OPENAI_BASE_URL", "http://gateway.invalid/v1")
	t.Setenv("OPENAI_API_KEY", "gateway-private-fixture")
	t.Setenv("OPENAI_MODEL", "heavy")
	c := New()
	c.Caller = "fixture-caller"
	cfg := map[string]string{"llm_url_standard": "http://fast-llm:8080/v1/", "llm_model_standard": "fast", "llm_model_oncall": "heavy", "llm_url_oncall": "http://retired.invalid/v1"}
	if err := c.Configure(cfg); err != nil {
		t.Fatal(err)
	}
	if c.URL != "http://fast-llm:8080/v1" || c.Model != "fast" || c.APIKey != "" || c.Caller != "fixture-caller" {
		t.Fatal("standard endpoint inherited a gateway key or lost caller identity")
	}
	if err := c.Configure(map[string]string{}); err != nil {
		t.Fatal(err)
	}
	if c.URL != "http://gateway.invalid/v1" || c.Model != "heavy" || c.APIKey != "gateway-private-fixture" {
		t.Fatal("empty configuration did not retain deployment provider")
	}
	cfg["llm_url_standard"] = "http://user:secret@elsewhere.invalid/v1"
	if err := c.Configure(cfg); err == nil {
		t.Fatal("embedded credentials accepted")
	}
}

func TestSavedPrismConnectionReconfiguresNewCallWithoutLeakingProviderKey(t *testing.T) {
	t.Setenv("OPENAI_BASE_URL", "http://gateway.invalid/v1")
	t.Setenv("OPENAI_API_KEY", "provider-secret-fixture")
	t.Setenv("OPENAI_MODEL", "legacy-model")
	t.Setenv("PRISM_URL", "http://old-prism.invalid")
	t.Setenv("PRISM_TOKEN", "old-service-token")
	c := New()
	c.Caller = "fixture-caller"
	raw := map[string]string{"prism_enabled": "true", "prism_url": "https://prism.invalid/prefix",
		"prism_token": "new-service-fixture", "prism_model": "source::model"}
	if err := c.Configure(raw); err != nil {
		t.Fatal(err)
	}
	if c.URL != "https://prism.invalid/prefix/api/voice/inference/v1" || c.Model != "source::model" ||
		c.APIKey != "new-service-fixture" || c.PrismURL != "https://prism.invalid/prefix" || c.PrismToken != "new-service-fixture" || c.Caller != "fixture-caller" {
		t.Fatal("saved connection was not applied as a call snapshot")
	}
	raw["prism_model"] = ""
	if err := c.Configure(raw); err != nil {
		t.Fatal(err)
	}
	if c.URL != "http://gateway.invalid/v1" || c.APIKey != "provider-secret-fixture" || !c.Linked() {
		t.Fatal("returning to legacy inference reused the Prism relay credential or disconnected identity")
	}
	raw["llm_url_standard"] = "http://other-llm.invalid/v1"
	if err := c.Configure(raw); err != nil {
		t.Fatal(err)
	}
	if c.APIKey != "" {
		t.Fatal("a different model URL inherited a provider or Prism credential")
	}
	delete(raw, "llm_url_standard")
	raw["prism_enabled"] = "false"
	if err := c.Configure(raw); err != nil {
		t.Fatal(err)
	}
	if c.URL != "http://gateway.invalid/v1" || c.Model != "legacy-model" || c.APIKey != "provider-secret-fixture" || c.Linked() || c.PrismToken != "" {
		t.Fatal("disabled saved connection did not override bootstrap connection")
	}
}
