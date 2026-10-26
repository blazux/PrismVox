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
