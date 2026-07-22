package webtool

import (
	"context"
	"net"
	"strings"
	"testing"
)

func TestValidateURLDomainAndAddressPolicy(t *testing.T) {
	publicLookup := func(context.Context, string, string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("8.8.8.8")}, nil
	}
	tests := []struct {
		name    string
		url     string
		allowed []string
		blocked []string
		lookup  func(context.Context, string, string) ([]net.IP, error)
		wantErr string
	}{
		{name: "exact allowed", url: "https://example.com/a", allowed: []string{"example.com"}, lookup: publicLookup},
		{name: "subdomain allowed", url: "https://docs.example.com", allowed: []string{"example.com"}, lookup: publicLookup},
		{name: "blocked wins", url: "https://docs.example.com", allowed: []string{"example.com"}, blocked: []string{"docs.example.com"}, lookup: publicLookup, wantErr: "blocked"},
		{name: "outside allowlist", url: "https://example.net", allowed: []string{"example.com"}, lookup: publicLookup, wantErr: "outside allowed_domains"},
		{name: "credentials", url: "https://user:secret@example.com", lookup: publicLookup, wantErr: "credentials"},
		{name: "loopback IPv4", url: "http://127.0.0.1", lookup: publicLookup, wantErr: "special-purpose"},
		{name: "private IPv4", url: "http://10.0.0.7", lookup: publicLookup, wantErr: "special-purpose"},
		{name: "loopback IPv6", url: "http://[::1]", lookup: publicLookup, wantErr: "special-purpose"},
		{name: "DNS private", url: "https://public.example", lookup: func(context.Context, string, string) ([]net.IP, error) {
			return []net.IP{net.ParseIP("93.184.216.34"), net.ParseIP("192.168.1.10")}, nil
		}, wantErr: "resolves to a special-purpose"},
		{name: "scheme", url: "file:///etc/passwd", lookup: publicLookup, wantErr: "absolute HTTP(S)"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateURL(context.Background(), test.url, test.allowed, test.blocked, test.lookup)
			if test.wantErr == "" && err != nil {
				t.Fatal(err)
			}
			if test.wantErr != "" && (err == nil || !strings.Contains(err.Error(), test.wantErr)) {
				t.Fatalf("err=%v want substring %q", err, test.wantErr)
			}
		})
	}
}
