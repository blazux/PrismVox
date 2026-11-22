package llm

import (
	"fmt"
	"net/url"
	"strings"

	"phoneagent/internal/secrets"
)

// Configure uses one Vox model for incoming/outgoing calls and auxiliary tasks.
// The existing standard keys remain canonical so upgrades preserve the inbound
// endpoint/model pair. Never forward a gateway key to another endpoint.
func (c *Client) Configure(raw map[string]string) error {
	base := New()
	c.URL, c.APIKey, c.Model = base.URL, base.APIKey, base.Model
	c.PrismURL, c.PrismToken = base.PrismURL, base.PrismToken
	if model := strings.TrimSpace(raw["llm_model_standard"]); model != "" {
		c.Model = model
	}
	if endpoint := strings.TrimRight(strings.TrimSpace(raw["llm_url_standard"]), "/"); endpoint != "" {
		u, err := url.Parse(endpoint)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return fmt.Errorf("invalid Vox model API URL")
		}
		if endpoint != strings.TrimRight(c.URL, "/") {
			c.APIKey = ""
		}
		c.URL = endpoint
	}
	if value, saved := raw["prism_enabled"]; saved {
		if value != "true" && value != "false" {
			return fmt.Errorf("invalid Prism connection state")
		}
		if value == "false" {
			c.PrismURL, c.PrismToken = "", ""
		} else {
			if value, ok := raw["prism_url"]; ok {
				endpoint := strings.TrimRight(strings.TrimSpace(value), "/")
				if endpoint != strings.TrimRight(c.PrismURL, "/") {
					c.PrismToken = ""
				}
				c.PrismURL = endpoint
			}
			if value, ok := raw["prism_token"]; ok {
				token, err := secrets.Decrypt(value, "prism_token")
				if err != nil {
					return fmt.Errorf("Prism connection credential unavailable")
				}
				c.PrismToken = token
			}
		}
	}
	if c.PrismURL != "" {
		if _, err := prismHTTPURL(c.PrismURL, "/api/voice/capabilities"); err != nil {
			return fmt.Errorf("invalid Prism connection URL")
		}
		if model := strings.TrimSpace(raw["prism_model"]); model != "" {
			if c.PrismToken == "" {
				return fmt.Errorf("Prism connection token is required for its model")
			}
			endpoint, err := prismHTTPURL(c.PrismURL, "/api/voice/inference/v1")
			if err != nil {
				return fmt.Errorf("invalid Prism inference URL")
			}
			c.URL, c.APIKey, c.Model = endpoint, c.PrismToken, model
		}
	}
	return nil
}
