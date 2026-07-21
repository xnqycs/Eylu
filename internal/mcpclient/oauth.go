package mcpclient

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

const (
	defaultOAuthCallbackTimeout = 5 * time.Minute
	oauthRefreshSkew            = 30 * time.Second
	maxOAuthResponseBytes       = 1 << 20
)

var ErrAuthorizationRequired = errors.New("MCP OAuth authorization required")

type OAuthOptions struct {
	ServerName          string
	ResourceURL         string
	ResourceMetadataURL string
	Issuer              string
	ClientID            string
	ClientSecret        string
	Scopes              []string
	RedirectURL         string
	CallbackTimeout     time.Duration
}

type OAuthClientOption func(*OAuthClient)

type OAuthClient struct {
	store       *CredentialStore
	httpClient  *http.Client
	openBrowser func(string) error
	output      io.Writer
	now         func() time.Time
	random      io.Reader
	refreshes   singleflight.Group
	pendingMu   sync.Mutex
	pending     map[string]pendingAuthorization
}

type pendingAuthorization struct {
	Verifier    string
	RedirectURL string
	Credential  string
	ExpiresAt   time.Time
}

func NewOAuthClient(store *CredentialStore, options ...OAuthClientOption) *OAuthClient {
	client := &OAuthClient{
		store:       store,
		httpClient:  http.DefaultClient,
		openBrowser: openBrowser,
		output:      os.Stderr,
		now:         time.Now,
		random:      rand.Reader,
		pending:     make(map[string]pendingAuthorization),
	}
	for _, option := range options {
		if option != nil {
			option(client)
		}
	}
	return client
}

func WithOAuthHTTPClient(httpClient *http.Client) OAuthClientOption {
	return func(client *OAuthClient) {
		if httpClient != nil {
			client.httpClient = httpClient
		}
	}
}

func WithOAuthBrowserOpener(opener func(string) error) OAuthClientOption {
	return func(client *OAuthClient) { client.openBrowser = opener }
}

func WithOAuthOutput(output io.Writer) OAuthClientOption {
	return func(client *OAuthClient) {
		if output != nil {
			client.output = output
		}
	}
}

func WithOAuthClock(now func() time.Time) OAuthClientOption {
	return func(client *OAuthClient) {
		if now != nil {
			client.now = now
		}
	}
}

func (o OAuthOptions) AuthHash() (string, error) {
	issuer := ""
	var err error
	if strings.TrimSpace(o.Issuer) != "" {
		issuer, err = normalizeIssuer(o.Issuer)
		if err != nil {
			return "", err
		}
	}
	redirect := ""
	if strings.TrimSpace(o.RedirectURL) != "" {
		redirect, err = normalizeLoopbackRedirect(o.RedirectURL, false)
		if err != nil {
			return "", err
		}
	}
	secretDigest := sha256.Sum256([]byte(o.ClientSecret))
	values := []string{issuer, strings.TrimSpace(o.ClientID), hex.EncodeToString(secretDigest[:]), strings.Join(normalizedScopes(o.Scopes), " "), redirect}
	digest := sha256.Sum256([]byte(lengthPrefixed(values...)))
	return hex.EncodeToString(digest[:]), nil
}

func (c *OAuthClient) credentialKey(options OAuthOptions) (string, error) {
	if c == nil || c.store == nil {
		return "", errors.New("MCP OAuth credential store is required")
	}
	authHash, err := options.AuthHash()
	if err != nil {
		return "", err
	}
	return CredentialKey(options.ServerName, options.ResourceURL, authHash)
}

func normalizeIssuer(rawURL string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return "", fmt.Errorf("parse OAuth issuer: %w", err)
	}
	if err := validateOAuthEndpoint(parsed, "OAuth issuer"); err != nil {
		return "", err
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" || parsed.User != nil {
		return "", errors.New("OAuth issuer must omit query, fragment, and userinfo")
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	parsed.Host = canonicalHost(parsed)
	return parsed.String(), nil
}

func normalizeLoopbackRedirect(rawURL string, requirePort bool) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return "", fmt.Errorf("parse OAuth redirect URL: %w", err)
	}
	if parsed.Scheme != "http" || parsed.Hostname() != "127.0.0.1" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("OAuth redirect URL must be an http://127.0.0.1 loopback URL without query, fragment, or userinfo")
	}
	if requirePort && parsed.Port() == "" {
		return "", errors.New("OAuth redirect URL requires a loopback port")
	}
	if parsed.Path == "" {
		parsed.Path = "/oauth/callback"
	}
	return parsed.String(), nil
}

func validateOAuthEndpoint(parsed *url.URL, label string) error {
	if parsed == nil || parsed.Host == "" {
		return fmt.Errorf("%s must contain a host", label)
	}
	if parsed.Scheme != "https" && !(parsed.Scheme == "http" && isLoopbackHostname(parsed.Hostname())) {
		return fmt.Errorf("%s must use HTTPS or loopback HTTP", label)
	}
	return nil
}

func canonicalHost(parsed *url.URL) string {
	hostname := strings.ToLower(parsed.Hostname())
	port := parsed.Port()
	if parsed.Scheme == "https" && port == "443" || parsed.Scheme == "http" && port == "80" {
		port = ""
	}
	if strings.Contains(hostname, ":") {
		hostname = "[" + hostname + "]"
	}
	if port != "" {
		return hostname + ":" + port
	}
	return hostname
}

func isLoopbackHostname(hostname string) bool {
	ip := net.ParseIP(strings.Trim(hostname, "[]"))
	return ip != nil && ip.IsLoopback()
}

func openBrowser(target string) error {
	var command *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		command = exec.Command("rundll32", "url.dll,FileProtocolHandler", target)
	case "darwin":
		command = exec.Command("open", target)
	default:
		command = exec.Command("xdg-open", target)
	}
	return command.Start()
}

func (c *OAuthClient) addPending(state string, pending pendingAuthorization) {
	c.pendingMu.Lock()
	defer c.pendingMu.Unlock()
	now := c.now()
	for key, value := range c.pending {
		if !value.ExpiresAt.After(now) {
			delete(c.pending, key)
		}
	}
	c.pending[state] = pending
}

func (c *OAuthClient) consumePending(state, callbackPath string) (pendingAuthorization, bool) {
	c.pendingMu.Lock()
	defer c.pendingMu.Unlock()
	pending, ok := c.pending[state]
	if !ok || !pending.ExpiresAt.After(c.now()) {
		delete(c.pending, state)
		return pendingAuthorization{}, false
	}
	redirect, err := url.Parse(pending.RedirectURL)
	if err != nil || redirect.EscapedPath() != callbackPath {
		return pendingAuthorization{}, false
	}
	delete(c.pending, state)
	return pending, true
}

func (c *OAuthClient) removePending(state string) {
	c.pendingMu.Lock()
	delete(c.pending, state)
	c.pendingMu.Unlock()
}

func (c *OAuthClient) randomURLString(bytes int) (string, error) {
	buffer := make([]byte, bytes)
	if _, err := io.ReadFull(c.random, buffer); err != nil {
		return "", err
	}
	return base64RawURL(buffer), nil
}

func base64RawURL(value []byte) string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	result := make([]byte, 0, (len(value)*8+5)/6)
	var accumulator uint32
	bits := 0
	for _, current := range value {
		accumulator = accumulator<<8 | uint32(current)
		bits += 8
		for bits >= 6 {
			bits -= 6
			result = append(result, alphabet[(accumulator>>bits)&0x3f])
		}
	}
	if bits > 0 {
		result = append(result, alphabet[(accumulator<<(6-bits))&0x3f])
	}
	return string(result)
}
