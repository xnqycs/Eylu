package logging

import (
	"regexp"
	"strings"
)

var secretPattern = regexp.MustCompile(`(?i)(sk-[a-z0-9_-]{12,}|bearer\s+[a-z0-9._~+/-]{12,}|(?:api[_-]?key|authorization|cookie)\s*[:=]\s*[^\s,;]+)`)

func Redact(value string, secrets ...string) string {
	result := value
	for _, secret := range secrets {
		if secret != "" {
			result = strings.ReplaceAll(result, secret, "[REDACTED]")
		}
	}
	return secretPattern.ReplaceAllString(result, "[REDACTED]")
}

func MaskSecret(secret string) string {
	if len(secret) <= 8 {
		return "********"
	}
	return secret[:3] + strings.Repeat("*", len(secret)-7) + secret[len(secret)-4:]
}
