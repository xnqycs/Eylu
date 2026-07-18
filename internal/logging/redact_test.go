package logging

import (
	"strings"
	"testing"
)

func TestRedact(t *testing.T) {
	secret := "sk-1234567890abcdef"
	got := Redact("Authorization: Bearer "+secret+" api_key="+secret, secret)
	if strings.Contains(got, secret) || !strings.Contains(got, "[REDACTED]") {
		t.Fatalf("redacted value = %q", got)
	}
	if masked := MaskSecret(secret); masked == secret || !strings.HasSuffix(masked, "cdef") {
		t.Fatalf("masked value = %q", masked)
	}
}
