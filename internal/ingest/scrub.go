package ingest

import (
	"encoding/json"
	"regexp"
	"strings"
)

// Redacted replaces sensitive values.
const Redacted = "[redacted]"

// The SDK sanitizes data before sending it (PRD §5). The server repeats a
// conservative pass so a misconfigured client can't store credentials
// (PRD §8: "Passwords, tokens and payment credentials must be redacted").
var sensitiveKeyRe = regexp.MustCompile(`(?i)(pass(word|wd)?|secret|token|api[-_]?key|auth|cookie|session|credential|card[-_]?(number|no)?|cc[-_]?num|cvv|cvc|iban|ssn|private[-_]?key)`)

var sensitiveHeaders = map[string]bool{
	"authorization":       true,
	"proxy-authorization": true,
	"cookie":              true,
	"set-cookie":          true,
	"x-api-key":           true,
	"x-faultbox-key":      true,
	"x-csrf-token":        true,
	"x-xsrf-token":        true,
	"php-auth-pw":         true,
}

// IsSensitiveKey reports whether a field name looks like it holds a secret.
func IsSensitiveKey(key string) bool { return sensitiveKeyRe.MatchString(key) }

// ScrubHeaders drops credential headers and redacts values under sensitive names.
func ScrubHeaders(h map[string]string) map[string]string {
	if len(h) == 0 {
		return nil
	}
	out := make(map[string]string, len(h))
	for k, v := range h {
		lk := strings.ToLower(k)
		switch {
		case sensitiveHeaders[lk]:
			continue
		case IsSensitiveKey(lk):
			out[k] = Redacted
		default:
			out[k] = v
		}
	}
	return out
}

// maxScrubDepth stops runaway recursion on deeply nested objects; anything
// deeper is replaced rather than stored unchecked.
const maxScrubDepth = 12

// ScrubJSON redacts values under sensitive keys anywhere in a JSON object.
// It returns nil for empty input.
func ScrubJSON(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	return json.Marshal(scrubValue(v, 0))
}

func scrubValue(v any, depth int) any {
	if depth > maxScrubDepth {
		return "[too deep]"
	}
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			if IsSensitiveKey(k) {
				if val != nil {
					t[k] = Redacted
				}
				continue
			}
			t[k] = scrubValue(val, depth+1)
		}
		return t
	case []any:
		for i := range t {
			t[i] = scrubValue(t[i], depth+1)
		}
		return t
	default:
		return v
	}
}
