package mcpclient

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

type oauthFixture struct {
	server       *httptest.Server
	resource     string
	issuer       string
	challenge    string
	registers    atomic.Int32
	codeTokens   atomic.Int32
	refreshes    atomic.Int32
	mu           sync.Mutex
	revoked      []string
	metadataHits atomic.Int32
}

func newOAuthFixture(t *testing.T) *oauthFixture {
	t.Helper()
	fixture := &oauthFixture{}
	handler := http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/.well-known/oauth-protected-resource/mcp":
			fixture.metadataHits.Add(1)
			writeFixtureJSON(t, response, map[string]any{
				"resource": fixture.resource, "authorization_servers": []string{fixture.issuer},
			})
		case "/.well-known/oauth-authorization-server/issuer":
			writeFixtureJSON(t, response, map[string]any{
				"issuer":                           fixture.issuer,
				"authorization_endpoint":           fixture.server.URL + "/authorize",
				"token_endpoint":                   fixture.server.URL + "/token",
				"registration_endpoint":            fixture.server.URL + "/register",
				"revocation_endpoint":              fixture.server.URL + "/revoke",
				"code_challenge_methods_supported": []string{"S256"},
				"response_types_supported":         []string{"code"},
				"grant_types_supported":            []string{"authorization_code", "refresh_token"},
				"scopes_supported":                 []string{"tools", "profile", "offline_access"},
			})
		case "/register":
			fixture.registers.Add(1)
			var body map[string]any
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Errorf("decode registration: %v", err)
			}
			if body["token_endpoint_auth_method"] != "none" {
				t.Errorf("registration auth method = %#v", body["token_endpoint_auth_method"])
			}
			response.Header().Set("Content-Type", "application/json")
			response.WriteHeader(http.StatusCreated)
			writeFixtureJSON(t, response, map[string]any{"client_id": "dynamic-client"})
		case "/token":
			if err := request.ParseForm(); err != nil {
				t.Errorf("parse token form: %v", err)
			}
			if request.Form.Get("resource") != fixture.resource {
				t.Errorf("token resource = %q", request.Form.Get("resource"))
			}
			switch request.Form.Get("grant_type") {
			case "authorization_code":
				fixture.codeTokens.Add(1)
				digest := sha256.Sum256([]byte(request.Form.Get("code_verifier")))
				if base64.RawURLEncoding.EncodeToString(digest[:]) != fixture.challenge {
					t.Error("token exchange PKCE verifier did not match challenge")
				}
				writeFixtureJSON(t, response, map[string]any{
					"access_token": "initial-access", "refresh_token": "initial-refresh", "token_type": "Bearer", "expires_in": 3600, "scope": "tools profile",
				})
			case "refresh_token":
				fixture.refreshes.Add(1)
				time.Sleep(20 * time.Millisecond)
				writeFixtureJSON(t, response, map[string]any{
					"access_token": "refreshed-access", "token_type": "Bearer", "expires_in": 3600, "scope": "tools",
				})
			default:
				http.Error(response, "unexpected grant", http.StatusBadRequest)
			}
		case "/revoke":
			if err := request.ParseForm(); err != nil {
				t.Errorf("parse revoke form: %v", err)
			}
			fixture.mu.Lock()
			fixture.revoked = append(fixture.revoked, request.Form.Get("token"))
			fixture.mu.Unlock()
			response.WriteHeader(http.StatusOK)
		default:
			http.NotFound(response, request)
		}
	})
	fixture.server = httptest.NewServer(handler)
	fixture.resource = fixture.server.URL + "/mcp"
	fixture.issuer = fixture.server.URL + "/issuer"
	t.Cleanup(fixture.server.Close)
	return fixture
}

func writeFixtureJSON(t *testing.T, response http.ResponseWriter, value any) {
	t.Helper()
	response.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(response).Encode(value); err != nil {
		t.Errorf("write fixture JSON: %v", err)
	}
}

func TestOAuthAuthorizeUsesDiscoveryDynamicRegistrationPKCEAndLoopback(t *testing.T) {
	fixture := newOAuthFixture(t)
	store := NewCredentialStore(filepath.Join(t.TempDir(), "mcp_credentials.json"))
	opener := func(rawURL string) error {
		authorizationURL, err := url.Parse(rawURL)
		if err != nil {
			return err
		}
		if authorizationURL.Query().Get("code_challenge_method") != "S256" || authorizationURL.Query().Get("client_id") != "dynamic-client" || authorizationURL.Query().Get("resource") != fixture.resource {
			return fmt.Errorf("authorization query = %s", authorizationURL.RawQuery)
		}
		fixture.challenge = authorizationURL.Query().Get("code_challenge")
		redirect, err := url.Parse(authorizationURL.Query().Get("redirect_uri"))
		if err != nil {
			return err
		}
		if redirect.Hostname() != "127.0.0.1" || redirect.Port() == "" {
			return fmt.Errorf("redirect URI = %q", redirect.String())
		}
		query := redirect.Query()
		query.Set("code", "authorization-code")
		query.Set("state", authorizationURL.Query().Get("state"))
		redirect.RawQuery = query.Encode()
		go func() {
			response, getErr := http.Get(redirect.String())
			if getErr == nil {
				io.Copy(io.Discard, response.Body)
				response.Body.Close()
			}
		}()
		return nil
	}
	client := NewOAuthClient(store, WithOAuthHTTPClient(fixture.server.Client()), WithOAuthBrowserOpener(opener), WithOAuthOutput(io.Discard))
	options := OAuthOptions{ServerName: "fixture", ResourceURL: fixture.resource, Scopes: []string{"tools", "profile"}, CallbackTimeout: time.Second}
	credential, err := client.Authorize(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	if credential.AccessToken != "initial-access" || credential.RefreshToken != "initial-refresh" || credential.DynamicClient == nil || credential.DynamicClient.ClientID != "dynamic-client" {
		t.Fatalf("credential = %#v", credential)
	}
	if fixture.registers.Load() != 1 || fixture.codeTokens.Load() != 1 {
		t.Fatalf("registers=%d code_tokens=%d", fixture.registers.Load(), fixture.codeTokens.Load())
	}
	hash, err := options.AuthHash()
	if err != nil {
		t.Fatal(err)
	}
	key, err := CredentialKey(options.ServerName, options.ResourceURL, hash)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := store.Get(context.Background(), key)
	if err != nil || stored.AccessToken != credential.AccessToken {
		t.Fatalf("stored = %#v, %v", stored, err)
	}
}

func TestOAuthTokenRefreshesOnceAcrossConcurrentCallers(t *testing.T) {
	fixture := newOAuthFixture(t)
	store := NewCredentialStore(filepath.Join(t.TempDir(), "mcp_credentials.json"))
	client := NewOAuthClient(store, WithOAuthHTTPClient(fixture.server.Client()))
	options := OAuthOptions{ServerName: "fixture", ResourceURL: fixture.resource, ClientID: "pre-registered", Scopes: []string{"tools"}}
	hash, err := options.AuthHash()
	if err != nil {
		t.Fatal(err)
	}
	key, err := CredentialKey(options.ServerName, options.ResourceURL, hash)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(context.Background(), key, OAuthCredential{AccessToken: "expired", RefreshToken: "refresh", TokenType: "Bearer", Expiry: time.Now().Add(20 * time.Second)}); err != nil {
		t.Fatal(err)
	}
	const callers = 16
	var wait sync.WaitGroup
	errorsSeen := make(chan error, callers)
	for index := 0; index < callers; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			header, headerErr := client.AuthorizationHeader(context.Background(), options)
			if headerErr != nil {
				errorsSeen <- headerErr
				return
			}
			if header != "Bearer refreshed-access" {
				errorsSeen <- fmt.Errorf("header = %q", header)
			}
		}()
	}
	wait.Wait()
	close(errorsSeen)
	for err := range errorsSeen {
		t.Error(err)
	}
	if fixture.refreshes.Load() != 1 {
		t.Fatalf("refresh requests = %d", fixture.refreshes.Load())
	}
}

func TestOAuthRefreshTransactionCoordinatesIndependentClients(t *testing.T) {
	fixture := newOAuthFixture(t)
	path := filepath.Join(t.TempDir(), "mcp_credentials.json")
	firstStore := NewCredentialStore(path)
	secondStore := NewCredentialStore(path)
	options := OAuthOptions{ServerName: "fixture", ResourceURL: fixture.resource, ClientID: "pre-registered", Scopes: []string{"tools"}}
	hash, _ := options.AuthHash()
	key, _ := CredentialKey(options.ServerName, options.ResourceURL, hash)
	if err := firstStore.Put(context.Background(), key, OAuthCredential{AccessToken: "expired", RefreshToken: "refresh", TokenType: "Bearer", Expiry: time.Now().Add(10 * time.Second)}); err != nil {
		t.Fatal(err)
	}
	clients := []*OAuthClient{
		NewOAuthClient(firstStore, WithOAuthHTTPClient(fixture.server.Client())),
		NewOAuthClient(secondStore, WithOAuthHTTPClient(fixture.server.Client())),
	}
	start := make(chan struct{})
	errorsSeen := make(chan error, len(clients))
	var wait sync.WaitGroup
	for _, client := range clients {
		wait.Add(1)
		go func(client *OAuthClient) {
			defer wait.Done()
			<-start
			credential, err := client.Token(context.Background(), options)
			if err != nil {
				errorsSeen <- err
				return
			}
			if credential.AccessToken != "refreshed-access" {
				errorsSeen <- fmt.Errorf("access token = %q", credential.AccessToken)
			}
		}(client)
	}
	close(start)
	wait.Wait()
	close(errorsSeen)
	for err := range errorsSeen {
		t.Error(err)
	}
	if fixture.refreshes.Load() != 1 {
		t.Fatalf("cross-client refresh requests = %d", fixture.refreshes.Load())
	}
}

func TestOAuthLogoutRevokesBeforeClearing(t *testing.T) {
	fixture := newOAuthFixture(t)
	store := NewCredentialStore(filepath.Join(t.TempDir(), "mcp_credentials.json"))
	client := NewOAuthClient(store, WithOAuthHTTPClient(fixture.server.Client()))
	options := OAuthOptions{ServerName: "fixture", ResourceURL: fixture.resource, ClientID: "pre-registered"}
	hash, _ := options.AuthHash()
	key, _ := CredentialKey(options.ServerName, options.ResourceURL, hash)
	if err := store.Put(context.Background(), key, OAuthCredential{AccessToken: "access", RefreshToken: "refresh", TokenType: "Bearer"}); err != nil {
		t.Fatal(err)
	}
	if err := client.Logout(context.Background(), options); err != nil {
		t.Fatal(err)
	}
	fixture.mu.Lock()
	revoked := append([]string(nil), fixture.revoked...)
	fixture.mu.Unlock()
	if strings.Join(revoked, ",") != "refresh,access" {
		t.Fatalf("revoked = %v", revoked)
	}
	if _, err := store.Get(context.Background(), key); err != ErrCredentialsNotFound {
		t.Fatalf("credential remained after logout: %v", err)
	}
}

func TestOAuthLogoutAggregatesRevocationFailuresAndClearsCredentials(t *testing.T) {
	var server *httptest.Server
	var mu sync.Mutex
	var attempts []string
	server = httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/stored-resource-metadata":
			writeFixtureJSON(t, response, map[string]any{
				"resource": server.URL + "/mcp", "authorization_servers": []string{server.URL + "/issuer"},
			})
		case "/.well-known/oauth-protected-resource/mcp":
			http.Error(response, "stored resource metadata URL was ignored", http.StatusTeapot)
		case "/.well-known/oauth-authorization-server/issuer":
			writeFixtureJSON(t, response, map[string]any{
				"issuer":                 server.URL + "/issuer",
				"authorization_endpoint": server.URL + "/authorize",
				"token_endpoint":         server.URL + "/token",
				"revocation_endpoint":    server.URL + "/revoke",
			})
		case "/revoke":
			if err := request.ParseForm(); err != nil {
				t.Errorf("parse revoke form: %v", err)
			}
			hint := request.Form.Get("token_type_hint")
			mu.Lock()
			attempts = append(attempts, hint)
			mu.Unlock()
			if hint == "refresh_token" {
				http.Error(response, "refresh revocation unavailable", http.StatusServiceUnavailable)
				return
			}
			http.Error(response, "access revocation unavailable", http.StatusBadGateway)
		default:
			http.NotFound(response, request)
		}
	}))
	t.Cleanup(server.Close)

	store := NewCredentialStore(filepath.Join(t.TempDir(), "mcp_credentials.json"))
	client := NewOAuthClient(store, WithOAuthHTTPClient(server.Client()))
	options := OAuthOptions{ServerName: "fixture", ResourceURL: server.URL + "/mcp", ClientID: "pre-registered"}
	hash, _ := options.AuthHash()
	key, _ := CredentialKey(options.ServerName, options.ResourceURL, hash)
	credential := OAuthCredential{
		AccessToken: "access-secret", RefreshToken: "refresh-secret", TokenType: "Bearer",
		ResourceMetadataURL: server.URL + "/stored-resource-metadata",
	}
	if err := store.Put(context.Background(), key, credential); err != nil {
		t.Fatal(err)
	}

	err := client.Logout(context.Background(), options)
	if err == nil {
		t.Fatal("Logout error = nil, want aggregated revocation diagnostics")
	}
	message := err.Error()
	for _, expected := range []string{"refresh_token", "503", "access_token", "502"} {
		if !strings.Contains(message, expected) {
			t.Errorf("Logout error %q omitted %q", message, expected)
		}
	}
	for _, secret := range []string{credential.AccessToken, credential.RefreshToken} {
		if strings.Contains(message, secret) {
			t.Errorf("Logout error exposed token %q", secret)
		}
	}
	mu.Lock()
	gotAttempts := append([]string(nil), attempts...)
	mu.Unlock()
	if strings.Join(gotAttempts, ",") != "refresh_token,access_token" {
		t.Fatalf("revocation attempts = %v", gotAttempts)
	}
	if _, err := store.Get(context.Background(), key); !errors.Is(err, ErrCredentialsNotFound) {
		t.Fatalf("credential remained after failed revocations: %v", err)
	}
}

func TestOAuthLogoutClearsCredentialsWhenDiscoveryFails(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		http.Error(response, "metadata unavailable", http.StatusServiceUnavailable)
	}))
	t.Cleanup(server.Close)

	store := NewCredentialStore(filepath.Join(t.TempDir(), "mcp_credentials.json"))
	client := NewOAuthClient(store, WithOAuthHTTPClient(server.Client()))
	options := OAuthOptions{ServerName: "fixture", ResourceURL: server.URL + "/mcp", ClientID: "pre-registered"}
	hash, _ := options.AuthHash()
	key, _ := CredentialKey(options.ServerName, options.ResourceURL, hash)
	if err := store.Put(context.Background(), key, OAuthCredential{AccessToken: "access", RefreshToken: "refresh", TokenType: "Bearer"}); err != nil {
		t.Fatal(err)
	}

	err := client.Logout(context.Background(), options)
	if err == nil || !strings.Contains(err.Error(), "discover") || !strings.Contains(err.Error(), "503") {
		t.Fatalf("Logout error = %v, want discovery diagnostic", err)
	}
	if _, err := store.Get(context.Background(), key); !errors.Is(err, ErrCredentialsNotFound) {
		t.Fatalf("credential remained after failed discovery: %v", err)
	}
}

func TestOAuthLogoutWaitsForLocalCleanupAfterCancellation(t *testing.T) {
	store := NewCredentialStore(filepath.Join(t.TempDir(), "mcp_credentials.json"))
	client := NewOAuthClient(store)
	options := OAuthOptions{ServerName: "fixture", ResourceURL: "http://127.0.0.1:1/mcp", ClientID: "pre-registered"}
	hash, _ := options.AuthHash()
	key, _ := CredentialKey(options.ServerName, options.ResourceURL, hash)
	if err := store.Put(context.Background(), key, OAuthCredential{AccessToken: "access", TokenType: "Bearer"}); err != nil {
		t.Fatal(err)
	}

	unlock, err := acquireCredentialFileLock(context.Background(), store.Path()+".lock")
	if err != nil {
		t.Fatal(err)
	}
	released := make(chan struct{})
	go func() {
		time.Sleep(50 * time.Millisecond)
		_ = unlock()
		close(released)
	}()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err = client.Logout(ctx, options)
	<-released
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("Logout error = %v, want context cancellation diagnostic", err)
	}
	if _, err := store.Get(context.Background(), key); !errors.Is(err, ErrCredentialsNotFound) {
		t.Fatalf("credential remained after canceled logout: %v", err)
	}
}

func TestOAuthDiscoveryRejectsMismatchedResourceMetadata(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		writeFixtureJSON(t, response, map[string]any{"resource": "https://attacker.example/mcp", "authorization_servers": []string{"https://login.example"}})
	}))
	defer server.Close()
	client := NewOAuthClient(NewCredentialStore(filepath.Join(t.TempDir(), "credentials.json")), WithOAuthHTTPClient(server.Client()))
	_, err := client.Discover(context.Background(), OAuthOptions{ServerName: "fixture", ResourceURL: server.URL + "/mcp"})
	if err == nil || !strings.Contains(err.Error(), "resource") {
		t.Fatalf("Discover error = %v", err)
	}
}

func TestParseResourceMetadataURL(t *testing.T) {
	for _, header := range []string{
		`Bearer realm="mcp", error="invalid_token", resource_metadata="https://mcp.example/.well-known/oauth-protected-resource"`,
		`Bearer resource_metadata="https://mcp.example/.well-known/oauth-protected-resource"`,
	} {
		got, ok := ParseResourceMetadataURL(header)
		if !ok || got != "https://mcp.example/.well-known/oauth-protected-resource" {
			t.Fatalf("ParseResourceMetadataURL(%q) = %q, %v", header, got, ok)
		}
	}
}

func TestNormalizeResourceURLRejectsLookalikeLoopbackHost(t *testing.T) {
	if _, err := NormalizeResourceURL("http://127.example/mcp"); err == nil {
		t.Fatal("lookalike loopback hostname was accepted over HTTP")
	}
}

func TestOAuthCallbackRejectsWrongPathWithoutConsumingState(t *testing.T) {
	client := NewOAuthClient(NewCredentialStore(filepath.Join(t.TempDir(), "credentials.json")))
	client.addPending("expected-state", pendingAuthorization{
		Verifier: "verifier", RedirectURL: "http://127.0.0.1:32123/oauth/callback", ExpiresAt: time.Now().Add(time.Minute),
	})
	results := make(chan oauthCallbackResult, 1)
	handler := client.oauthCallbackHandler(results)
	wrong := httptest.NewRecorder()
	handler.ServeHTTP(wrong, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:32123/wrong?state=expected-state&code=wrong", nil))
	if wrong.Code != http.StatusBadRequest {
		t.Fatalf("wrong callback path status = %d", wrong.Code)
	}
	correct := httptest.NewRecorder()
	handler.ServeHTTP(correct, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:32123/oauth/callback?state=expected-state&code=correct", nil))
	if correct.Code != http.StatusOK {
		t.Fatalf("correct callback status = %d", correct.Code)
	}
	select {
	case result := <-results:
		if result.code != "correct" {
			t.Fatalf("callback code = %q", result.code)
		}
	default:
		t.Fatal("correct callback did not produce a result")
	}
}

func TestSDKHandlerPersistsHooksRestoresAndRefreshes(t *testing.T) {
	fixture := newOAuthFixture(t)
	store := NewCredentialStore(filepath.Join(t.TempDir(), "mcp_credentials.json"))
	opener := func(rawURL string) error {
		authorizationURL, err := url.Parse(rawURL)
		if err != nil {
			return err
		}
		if authorizationURL.Query().Get("code_challenge_method") != "S256" || authorizationURL.Query().Get("client_id") != "dynamic-client" {
			return fmt.Errorf("SDK authorization query = %s", authorizationURL.RawQuery)
		}
		if !strings.Contains(authorizationURL.Query().Get("scope"), "tools") {
			return fmt.Errorf("SDK authorization scope = %q", authorizationURL.Query().Get("scope"))
		}
		fixture.challenge = authorizationURL.Query().Get("code_challenge")
		redirect, err := url.Parse(authorizationURL.Query().Get("redirect_uri"))
		if err != nil {
			return err
		}
		query := redirect.Query()
		query.Set("code", "authorization-code")
		query.Set("state", authorizationURL.Query().Get("state"))
		redirect.RawQuery = query.Encode()
		go func() {
			response, getErr := http.Get(redirect.String())
			if getErr == nil {
				_, _ = io.Copy(io.Discard, response.Body)
				_ = response.Body.Close()
			}
		}()
		return nil
	}
	client := NewOAuthClient(store, WithOAuthHTTPClient(fixture.server.Client()), WithOAuthBrowserOpener(opener), WithOAuthOutput(io.Discard))
	options := OAuthOptions{ServerName: "fixture", ResourceURL: fixture.resource, Scopes: []string{"tools"}, CallbackTimeout: time.Second}
	handler, err := client.SDKHandler(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	transport := &sdkmcp.StreamableClientTransport{Endpoint: fixture.resource, HTTPClient: fixture.server.Client(), OAuthHandler: handler}
	if transport.OAuthHandler == nil {
		t.Fatal("SDK OAuth handler was not accepted by StreamableClientTransport")
	}
	request := httptest.NewRequest(http.MethodGet, fixture.resource, nil)
	response := &http.Response{StatusCode: http.StatusUnauthorized, Header: make(http.Header), Body: http.NoBody, Request: request}
	metadataURL, err := ProtectedResourceMetadataURL(fixture.resource)
	if err != nil {
		t.Fatal(err)
	}
	challengeMetadataURL := metadataURL + "?source=challenge"
	response.Header.Set("WWW-Authenticate", `Bearer resource_metadata="`+challengeMetadataURL+`", scope="tools"`)
	if err := handler.Authorize(context.Background(), request, response); err != nil {
		t.Fatal(err)
	}
	tokenSource, err := handler.TokenSource(context.Background())
	if err != nil || tokenSource == nil {
		t.Fatalf("SDK TokenSource = %#v, %v", tokenSource, err)
	}
	token, err := tokenSource.Token()
	if err != nil || token.AccessToken != "initial-access" {
		t.Fatalf("SDK token = %#v, %v", token, err)
	}
	hash, _ := options.AuthHash()
	key, _ := CredentialKey(options.ServerName, options.ResourceURL, hash)
	stored, err := store.Get(context.Background(), key)
	if err != nil || stored.DynamicClient == nil || stored.DynamicClient.ClientID != "dynamic-client" || stored.ResourceMetadataURL != challengeMetadataURL {
		t.Fatalf("SDK stored credential = %#v, %v", stored, err)
	}
	stored.Expiry = time.Now().Add(10 * time.Second)
	if err := store.Put(context.Background(), key, stored); err != nil {
		t.Fatal(err)
	}
	restoredClient := NewOAuthClient(store, WithOAuthHTTPClient(fixture.server.Client()), WithOAuthBrowserOpener(func(string) error {
		return errors.New("restored SDK token source opened a browser")
	}), WithOAuthOutput(io.Discard))
	restoredHandler, err := restoredClient.SDKHandler(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	restoredSource, err := restoredHandler.TokenSource(context.Background())
	if err != nil || restoredSource == nil {
		t.Fatalf("restored SDK TokenSource = %#v, %v", restoredSource, err)
	}
	refreshed, err := restoredSource.Token()
	if err != nil || refreshed.AccessToken != "refreshed-access" {
		t.Fatalf("refreshed SDK token = %#v, %v", refreshed, err)
	}
	if fixture.refreshes.Load() != 1 {
		t.Fatalf("SDK refresh requests = %d", fixture.refreshes.Load())
	}
}
