// Package httpguard keeps private API requests at their configured origin.
package httpguard

import (
	"errors"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
)

var ErrRedirectOrigin = errors.New("API redirect outside configured origin refused")
var ErrRedirectLimit = errors.New("too many API redirects")

type origin struct{ scheme, host, port string }

func canonicalOrigin(u *url.URL) (origin, bool) {
	if u == nil {
		return origin{}, false
	}
	o := origin{strings.ToLower(u.Scheme), u.Hostname(), u.Port()}
	if o.host == "" || (o.scheme != "http" && o.scheme != "https") {
		return origin{}, false
	}
	if address, err := netip.ParseAddr(o.host); err == nil {
		o.host = address.String() // IPv6 zones retain their case-sensitive interface name.
	} else {
		o.host = strings.ToLower(o.host)
	}
	if o.port == "" {
		o.port = "80"
		if o.scheme == "https" {
			o.port = "443"
		}
	} else {
		port, err := strconv.ParseUint(o.port, 10, 16)
		if err != nil {
			return origin{}, false
		}
		o.port = strconv.FormatUint(port, 10)
	}
	return o, true
}

// SameOriginRedirect permits path canonicalization, never a new recipient.
func SameOriginRedirect(request *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return ErrRedirectLimit
	}
	if len(via) == 0 || request == nil || request.URL == nil || via[0] == nil {
		return ErrRedirectOrigin
	}
	initial, validInitial := canonicalOrigin(via[0].URL)
	next, validNext := canonicalOrigin(request.URL)
	if request.URL.User != nil || !validInitial || !validNext || initial != next {
		return ErrRedirectOrigin
	}
	return nil
}

// Do removes net/http's URL wrapper from policy failures: a Location may itself
// contain credentials or confidential query values.
func Do(client *http.Client, request *http.Request) (*http.Response, error) {
	response, err := client.Do(request)
	for _, policy := range []error{ErrRedirectOrigin, ErrRedirectLimit} {
		if errors.Is(err, policy) {
			return response, policy
		}
	}
	return response, err
}
