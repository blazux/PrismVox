package llm

import (
	"fmt"
	"net/url"
	"strings"
)

// ConfigureTier keeps the deployment gateway as the default and optionally uses
// a different API root per tier. Never forward a gateway key to another endpoint.
func (c *Client) ConfigureTier(raw map[string]string, tier string) error {
	base := New()
	c.URL, c.APIKey, c.Model = base.URL, base.APIKey, base.Model
	if model := strings.TrimSpace(raw["llm_model_"+tier]); model != "" {
		c.Model = model
	}
	if endpoint := strings.TrimRight(strings.TrimSpace(raw["llm_url_"+tier]), "/"); endpoint != "" {
		u, err := url.Parse(endpoint)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return fmt.Errorf("invalid %s model API URL", tier)
		}
		if endpoint != strings.TrimRight(c.URL, "/") {
			c.APIKey = ""
		}
		c.URL = endpoint
	}
	return nil
}
