package obs

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// OTLP endpoint and header settings, validated as otel-config.ts does.

// ValidateEndpoint returns a problem with an OTLP/HTTP base endpoint, or "".
func ValidateEndpoint(v string) string {
	u, err := url.Parse(v)
	if err != nil || u.Scheme == "" || u.Host == "" || len(v) > 2048 {
		return "Invalid URL"
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "must use http or https"
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" || strings.Contains(v, "?") || strings.Contains(v, "#") {
		return "must not contain credentials, query parameters, or a fragment; use OTLP headers for authentication"
	}
	return ""
}

// SanitizedEndpoint is the endpoint without anything sensitive, or "".
func SanitizedEndpoint(v string) string {
	if v == "" || ValidateEndpoint(v) != "" {
		return ""
	}
	u, _ := url.Parse(v)
	path := u.Path
	if path == "" {
		path = "/" // WHATWG URL serialization, as TS reports it
	}
	return u.Scheme + "://" + whatwgHost(u) + path
}

var (
	headerNameRe   = regexp.MustCompile("^[!#$%&'*+\\-.^_`|~0-9A-Za-z]+$")
	headerCtrlRe   = regexp.MustCompile("[\x00-\x08\x0a-\x1f\x7f]")
	transportOwned = map[string]bool{"connection": true, "content-encoding": true, "content-length": true,
		"content-type": true, "expect": true, "host": true, "keep-alive": true, "proxy-connection": true, "te": true,
		"trailer": true, "transfer-encoding": true, "upgrade": true}
)

// ParseHeaders parses `key=value,...` OTLP exporter headers (values may be
// percent-encoded).
func ParseHeaders(v string) (map[string]string, error) {
	out := map[string]string{}
	if strings.TrimSpace(v) == "" {
		return out, nil
	}
	if len(v) > 8192 {
		return nil, errors.New("OTLP headers are too long.")
	}
	for _, entry := range strings.Split(v, ",") {
		sep := strings.Index(entry, "=")
		if sep < 1 {
			return nil, errors.New("OTLP headers must use key=value.")
		}
		name := strings.TrimSpace(entry[:sep])
		if len(name) > 128 || !headerNameRe.MatchString(name) {
			return nil, fmt.Errorf("OTLP header name %q is invalid.", name)
		}
		name = strings.ToLower(name)
		if _, dup := out[name]; dup {
			return nil, fmt.Errorf("OTLP header %s is duplicated.", name)
		}
		if transportOwned[name] {
			return nil, fmt.Errorf("OTLP header %s is managed by the transport.", name)
		}
		decoded, err := url.PathUnescape(strings.TrimSpace(entry[sep+1:]))
		if err != nil {
			return nil, fmt.Errorf("OTLP header %s is not valid percent-encoding.", name)
		}
		if decoded == "" || len(decoded) > 4096 || headerCtrlRe.MatchString(decoded) {
			return nil, fmt.Errorf("OTLP header %s has an invalid value.", name)
		}
		out[name] = decoded
	}
	return out, nil
}

// whatwgHost is the host as a WHATWG URL serializes it: lowercase, without
// the scheme's default port.
func whatwgHost(u *url.URL) string {
	host := strings.ToLower(u.Host)
	if p := u.Port(); (u.Scheme == "http" && p == "80") || (u.Scheme == "https" && p == "443") {
		host = strings.TrimSuffix(host, ":"+p)
	}
	return host
}
