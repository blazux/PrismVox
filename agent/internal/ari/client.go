// Package ari provides an HTTP client for the Asterisk ARI REST API.
package ari

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client is a thin HTTP wrapper for ARI REST calls with Basic Auth.
type Client struct {
	BaseURL string
	user    string
	pass    string
	http    *http.Client
}

// New returns a new ARI Client.
func New(baseURL, user, pass string) *Client {
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		user:    user,
		pass:    pass,
		http: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

func (c *Client) newRequest(method, path string, body io.Reader) (*http.Request, error) {
	u := c.BaseURL + path
	req, err := http.NewRequest(method, u, body)
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(c.user, c.pass)
	return req, nil
}

// Post sends a POST with JSON body, returns raw response bytes and status code.
func (c *Client) Post(path string, body interface{}) ([]byte, int, error) {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, 0, err
		}
		r = bytes.NewReader(b)
	}
	req, err := c.newRequest("POST", path, r)
	if err != nil {
		return nil, 0, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	return data, resp.StatusCode, err
}

// PostParams sends a POST with URL query parameters (no body).
func (c *Client) PostParams(path string, params url.Values) ([]byte, int, error) {
	fullPath := path
	if len(params) > 0 {
		fullPath = path + "?" + params.Encode()
	}
	req, err := c.newRequest("POST", fullPath, nil)
	if err != nil {
		return nil, 0, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	return data, resp.StatusCode, err
}

// Get sends a GET request and returns the raw response bytes and status code.
func (c *Client) Get(path string) ([]byte, int, error) {
	req, err := c.newRequest("GET", path, nil)
	if err != nil {
		return nil, 0, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	return data, resp.StatusCode, err
}

// LiveChannelIDs returns the IDs of all channels Asterisk currently knows about.
// Used to reconcile the agent's active-call bookkeeping after an ARI reconnect.
func (c *Client) LiveChannelIDs() (map[string]struct{}, error) {
	data, status, err := c.Get("/channels")
	if err != nil {
		return nil, err
	}
	if status >= 400 {
		return nil, fmt.Errorf("ARI GET /channels: status %d", status)
	}
	var chans []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(data, &chans); err != nil {
		return nil, fmt.Errorf("ARI GET /channels decode: %w", err)
	}
	ids := make(map[string]struct{}, len(chans))
	for _, ch := range chans {
		ids[ch.ID] = struct{}{}
	}
	return ids, nil
}

// Delete sends a DELETE request.
func (c *Client) Delete(path string) error {
	req, err := c.newRequest("DELETE", path, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body) //nolint:errcheck
	if resp.StatusCode >= 400 {
		return fmt.Errorf("ARI DELETE %s: status %d", path, resp.StatusCode)
	}
	return nil
}

// PostJSON sends a POST with JSON body and decodes the JSON response.
func (c *Client) PostJSON(path string, v interface{}) (map[string]interface{}, error) {
	data, status, err := c.Post(path, v)
	if err != nil {
		return nil, err
	}
	if status >= 400 {
		return nil, fmt.Errorf("ARI POST %s: status %d body=%s", path, status, string(data))
	}
	if len(data) == 0 {
		return nil, nil
	}
	var result map[string]interface{}
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, fmt.Errorf("ARI POST %s: decode error: %w (body=%s)", path, err, string(data))
	}
	return result, nil
}
