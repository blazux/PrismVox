package llm

import (
	"fmt"
	"net/url"
	"strings"
)

// Configure uses one Vox model for incoming/outgoing calls and auxiliary tasks.
// The existing standard keys remain canonical so upgrades preserve the inbound
// endpoint/model pair. Never forward a gateway key to another endpoint.
func (c *Client) Configure(raw map[string]string) error {
	base := New()
	c.URL, c.APIKey, c.Model = base.URL, base.APIKey, base.Model
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
	return nil
}
