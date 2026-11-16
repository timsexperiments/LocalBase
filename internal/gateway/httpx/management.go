package httpx

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"slices"
)

var managementMessages = map[string]string{
	"invalid_api_key":               "Authentication is required.",
	"insufficient_permissions":      "This identity cannot perform that action.",
	"validation_failed":             "The management request is invalid.",
	"payload_too_large":             "The management request exceeds the size limit.",
	"request_aborted":               "The management request was cancelled.",
	"provider_not_configured":       "Configure a browser identity provider first.",
	"email_delivery_not_configured": "Configure email delivery before enabling magic-link authentication.",
	"email_delivery_in_use":         "Remove the magic-link registration before disabling email delivery.",
	"email_delivery_unreadable":     "Unable to read email delivery configuration.",
	"email_delivery_failed":         "Email delivery failed.",
	"policy_not_configured":         "Configure a browser access policy first.",
	"registration_not_found":        "The identity provider registration was not found.",
	"key_not_found":                 "The API key was not found.",
	"managed_user_not_found":        "The managed user was not found.",
	"managed_user_exists":           "The managed user already exists.",
	"role_not_found":                "One or more browser access roles were not found.",
	"policy_conflict":               "The browser access policy cannot be changed.",
	"policy_revision_conflict":      "The browser access policy changed. Refresh it and try again.",
}

// ManagementError answers a management route's error in its own shape: code, and the message
// managementMessages gives it, never stored.
func ManagementError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Cache-Control", "no-store")
	JSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": managementMessages[code]}})
}

// ManagementErrorMessage is ManagementError with a message of the caller's own, for a code whose
// words depend on what happened (email_delivery_unreadable carries LoadEmail's).
func ManagementErrorMessage(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Cache-Control", "no-store")
	JSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": message}})
}

// NoStore answers v as JSON with status, never stored.
func NoStore(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Cache-Control", "no-store")
	JSON(w, status, v)
}

// MaxManagementBody is the most a management request's body may hold.
const MaxManagementBody = 64 * 1024

// ReadManagement reads a management request's body, which must be valid JSON within
// MaxManagementBody. When it isn't, it has answered, and reports false.
func ReadManagement(w http.ResponseWriter, r *http.Request) (json.RawMessage, bool) {
	if r.ContentLength > MaxManagementBody {
		ManagementError(w, 413, "payload_too_large")
		return nil, false
	}
	b, err := io.ReadAll(io.LimitReader(r.Body, MaxManagementBody+1))
	if err != nil {
		if r.Context().Err() != nil {
			ManagementError(w, 499, "request_aborted")
		} else {
			ManagementError(w, 400, "validation_failed")
		}
		return nil, false
	}
	if len(b) > MaxManagementBody {
		ManagementError(w, 413, "payload_too_large")
		return nil, false
	}
	if !json.Valid(b) {
		ManagementError(w, 400, "validation_failed")
		return nil, false
	}
	return b, true
}

// StrictDecode decodes JSON rejecting unknown fields.
func StrictDecode(b []byte, v any) bool {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	return dec.Decode(v) == nil
}

// OnlyFields reports a JSON object whose members other than "action" are all
// in fields and none is null (zod's strict, non-nullable objects).
func OnlyFields(b []byte, fields ...string) bool {
	var m map[string]json.RawMessage
	if json.Unmarshal(b, &m) != nil {
		return false
	}
	for k, v := range m {
		if k != "action" && (!slices.Contains(fields, k) || string(v) == "null") {
			return false
		}
	}
	return true
}
