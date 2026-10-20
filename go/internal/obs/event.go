package obs

import (
	"math"
	"regexp"
	"slices"
	"strings"
)

// Event sanitizing, ported from logging.ts createLogEvent: every event the
// sink writes satisfies the TS logEventSchema, so `local-base logs` from
// either binary reads files written by the other.

const (
	maxMessage        = 2048
	maxAttributes     = 32
	maxAttributeValue = 512
	maxEventName      = 128
	maxComponent      = 64
	maxRequestID      = 128
	redacted          = "[REDACTED]"
)

var (
	categories = []string{"gateway", "http", "runtime", "service", "logging"}
	runtimes   = []string{"gateway", "llm", "stt", "tts", "image", "video", "service", "cli"}
	// Routes TS recognizes in http.path (route-dispatch.ts gatewayHttpRoutes).
	apiRoutes = []string{"/health", "/health/ready", "/_localbase/instance", "/_localbase/access-management",
		"/_localbase/api-keys", "/_localbase/model-management", "/_localbase/model-discovery", "/_localbase/hf-token",
		"/_localbase/voices", "/_localbase/voices/{voice_id}", "/_localbase/voices/{voice_id}/audio",
		"/_localbase/models", "/_localbase/models/{model_id}", "/v1/models", "/v1/models/{model}",
		"/v1/chat/completions", "/v1/embeddings", "/v1/systemone", "/v1/audio/transcriptions",
		"/v1/audio/translations", "/v1/audio/speech", "/v1/images/generations", "/v1/images/edits", "/v1/videos",
		"/v1/videos/{job_id}", "/v1/videos/{job_id}/content", "/v1/videos/{job_id}/cancel"}
	uiRoutes = []string{"/", "/app", "/app/assets/{asset}", "/app/login", "/app/login/magic", "/app/logout",
		"/app/session", "/oidc/callback", "/github/callback", "/magic-link/callback"}

	sensitiveKey     = regexp.MustCompile(`(?i)(?:authorization|bearer|api[-_. ]?key|token|secret|password|credential|cookie|hf[-_. ]?token)`)
	contentKey       = regexp.MustCompile(`(?i)(?:content|prompt|messages?|input|body|response|completion|output)`)
	metricKeys       = []string{"prompt_tokens", "completion_tokens", "total_tokens", "prompt_duration_ms", "predicted_duration_ms"}
	bearerRe         = regexp.MustCompile(`(?i)\bBearer\s+[A-Za-z0-9._~+/-]+=*`)
	labeledSecretRe  = regexp.MustCompile(`(?i)\b(authorization|api[-_ ]?key|token|secret|password|hf[-_ ]?token)\s*[:=]\s*([^\s,;]+)`)
	knownTokenRe     = regexp.MustCompile(`(?i)\b(?:hf_[A-Za-z0-9_-]{8,}|lb_[A-Za-z0-9_-]{8,}|sk(?:-proj)?-[A-Za-z0-9_-]{8,}|gh[pousr]_[A-Za-z0-9]{12,}|AKIA[A-Z0-9]{16}|[A-Za-z0-9_-]*token[A-Za-z0-9_-]{12,})\b`)
	cookieRe         = regexp.MustCompile(`(?i)\b(?:set-cookie|cookie)\s*:\s*[^\r\n]+`)
	urlCredentialRe  = regexp.MustCompile(`(?i)([a-z][a-z0-9+.-]*://)[^@\s/]+@`)
	urlQuerySecretRe = regexp.MustCompile(`(?i)([?&](?:access_token|api[-_]?key|authorization|token|secret|password)=)[^&#\s]*`)
	embeddedContent  = regexp.MustCompile(`(?i)["']?(?:content|prompt|messages?|input|body|response|completion|output)["']?\s*[:=]`)
	ansiRe           = regexp.MustCompile("\x1b\\[[0-9;]*[A-Za-z]")
	attrKeyRe        = regexp.MustCompile(`(?i)^[a-z][a-z0-9_.-]*$`)
	requestIDRe      = regexp.MustCompile(`^[A-Za-z0-9._:-]+$`)
)

// BoundedText redacts credentials and request content and bounds the length.
func BoundedText(s string, max int) string {
	s = ansiRe.ReplaceAllString(s, "")
	if embeddedContent.MatchString(s) {
		return "[REDACTED REQUEST OR MODEL CONTENT]"
	}
	s = bearerRe.ReplaceAllString(s, "Bearer [REDACTED]")
	s = labeledSecretRe.ReplaceAllString(s, "$1=[REDACTED]")
	s = cookieRe.ReplaceAllString(s, "cookie: [REDACTED]")
	s = urlCredentialRe.ReplaceAllString(s, "$1[REDACTED]@")
	s = urlQuerySecretRe.ReplaceAllString(s, "$1[REDACTED]")
	s = knownTokenRe.ReplaceAllString(s, redacted)
	if r := []rune(s); len(r) > max {
		return string(r[:max-1]) + "…"
	}
	return s
}

func normalize(s string, allowed *regexp.Regexp, max int, fallback string) string {
	s = strings.ToLower(strings.TrimSpace(ansiRe.ReplaceAllString(s, "")))
	s = allowed.ReplaceAllString(s, "-")
	s = strings.Trim(s, "._-")
	if len(s) > max {
		s = strings.TrimRight(s[:max], "._-")
	}
	if s == "" || s[0] < 'a' || s[0] > 'z' {
		return fallback
	}
	return s
}

var (
	eventNameInvalid = regexp.MustCompile(`[^a-z0-9._-]+`)
	componentInvalid = regexp.MustCompile(`[^a-z0-9]+`)
)

func eventName(s string) string {
	return normalize(s, eventNameInvalid, maxEventName, "localbase.event")
}

func componentName(s string) string {
	return normalize(strings.ReplaceAll(s, ".", "-"), componentInvalid, maxComponent, "localbase")
}

// attribute returns the sanitized value for a key, or ok=false to drop it.
func attribute(key string, v any) (string, any, bool) {
	if !attrKeyRe.MatchString(key) || len(key) > 64 {
		return "", nil, false
	}
	k := strings.ToLower(key)
	if slices.Contains(metricKeys, k) {
		if f, ok := toFloat(v); ok && f >= 0 && !math.IsInf(f, 0) && !math.IsNaN(f) {
			return k, v, true
		}
		return k, redacted, true
	}
	if sensitiveKey.MatchString(key) || contentKey.MatchString(key) {
		return k, redacted, true
	}
	switch x := v.(type) {
	case string:
		return k, BoundedText(x, maxAttributeValue), true
	case bool:
		return k, x, true
	}
	if f, ok := toFloat(v); ok {
		if math.IsInf(f, 0) || math.IsNaN(f) {
			return "", nil, false
		}
		return k, v, true
	}
	return "", nil, false
}

func toFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case uint64:
		return float64(x), true
	case float64:
		return x, true
	case float32:
		return float64(x), true
	}
	return 0, false
}

// canonicalRoute maps a route label to a name TS recognizes. Pages of the
// server-rendered UI are recorded as "/app" (the UI shell route in TS).
func canonicalRoute(path string) (route, uiPage string) {
	switch {
	case slices.Contains(apiRoutes, path), slices.Contains(uiRoutes, path):
		return path, ""
	case path == "/_localbase/backends", path == "/_localbase/model-backend", path == "/_localbase/model-imports":
		// Go-only model APIs, recorded under the TS model-management route.
		return "/_localbase/model-management", path
	case strings.HasPrefix(path, "/app/static/"):
		return "/app/assets/{asset}", ""
	case path == "/app/" || strings.HasPrefix(path, "/app/"):
		return "/app", path
	}
	return "unmatched-route", ""
}

func safeRequestID(id string) string {
	if id == "" || len(id) > maxRequestID || !requestIDRe.MatchString(id) || sensitiveKey.MatchString(id) || knownTokenRe.MatchString(id) {
		return ""
	}
	return id
}

// categoryFor derives a valid category from an event name.
func categoryFor(event string) string {
	prefix, _, _ := strings.Cut(event, ".")
	switch prefix {
	case "http":
		return "http"
	case "runtime", "inference", "memory", "models":
		return "runtime"
	case "service":
		return "service"
	case "logging":
		return "logging"
	}
	return "gateway"
}
