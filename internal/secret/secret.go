// Package secret performs conservative, high-confidence credential detection
// at persistence boundaries. It deliberately does not return matched values.
package secret

import (
	"regexp"
	"strings"
)

var credentialPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\bsk-[a-z0-9_-]{20,}\b`),
	regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9_]{30,}\b`),
	regexp.MustCompile(`\bAKIA[A-Z0-9]{16}\b`),
	regexp.MustCompile(`(?i)\bbearer\s+[a-z0-9._~-]{20,}`),
	regexp.MustCompile(`(?i)["']?\b(password|passwd|api[_ -]?key|access[_ -]?token|refresh[_ -]?token|session[_ -]?token|cookie)\b["']?\s*[:=]\s*["']?[A-Za-z0-9+/_.~=-]{12,}`),
}

func LooksLikeCredential(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	text := string(body)
	for _, pattern := range credentialPatterns {
		if pattern.MatchString(text) {
			return true
		}
	}
	return strings.Contains(text, "-----BEGIN PRIVATE KEY-----") || strings.Contains(text, "-----BEGIN RSA PRIVATE KEY-----")
}
