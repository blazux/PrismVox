package httpguard

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestOriginNormalizationAndRedirectLimit(t *testing.T) {
	for _, test := range []struct {
		from, to string
		allowed  bool
	}{
		{"https://API.example.test/v1", "https://api.example.test:443/canonical", true},
		{"http://[::1]:80/v1", "http://[::1]/canonical", true},
		{"https://api.example.test", "https://api.example.test:0443", true},
		{"http://[fe80::1%25Eth0]", "http://[fe80::1%25eth0]", false},
		{"https://api.example.test", "http://api.example.test", false},
		{"https://api.example.test", "https://api.example.test:444", false},
		{"https://api.example.test", "https://other.example.test", false},
		{"https://api.example.test", "https://user:private@api.example.test", false},
	} {
		from, _ := url.Parse(test.from)
		to, _ := url.Parse(test.to)
		err := SameOriginRedirect(&http.Request{URL: to}, []*http.Request{{URL: from}})
		if (err == nil) != test.allowed {
			t.Fatalf("allowed=%t error=%v", test.allowed, err)
		}
	}
	u, _ := url.Parse("https://api.example.test")
	via := make([]*http.Request, 10)
	if !errors.Is(SameOriginRedirect(&http.Request{URL: u}, via), ErrRedirectLimit) {
		t.Fatal("redirect loop limit not retained")
	}
}

func TestPolicyErrorDoesNotExposeRedirectURL(t *testing.T) {
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "https://private-user:private-password@foreign.example.test/?key=private-key")
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer source.Close()
	request, _ := http.NewRequest("POST", source.URL, strings.NewReader("private-body"))
	_, err := Do(&http.Client{CheckRedirect: SameOriginRedirect}, request)
	if !errors.Is(err, ErrRedirectOrigin) || strings.Contains(err.Error(), "private-") {
		t.Fatalf("policy failure leaked redirect data: %v", err)
	}
}
