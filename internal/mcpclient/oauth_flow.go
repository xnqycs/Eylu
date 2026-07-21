package mcpclient

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type oauthCallbackResult struct {
	code    string
	issuer  string
	pending pendingAuthorization
	err     error
}

type oauthTokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token,omitempty"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in,omitempty"`
	Scope        string `json:"scope,omitempty"`
}

type dynamicRegistrationResponse struct {
	ClientID                string `json:"client_id"`
	ClientSecret            string `json:"client_secret,omitempty"`
	TokenEndpointAuthMethod string `json:"token_endpoint_auth_method,omitempty"`
	ClientIDIssuedAt        int64  `json:"client_id_issued_at,omitempty"`
	ClientSecretExpiresAt   int64  `json:"client_secret_expires_at,omitempty"`
	RegistrationAccessToken string `json:"registration_access_token,omitempty"`
	RegistrationClientURI   string `json:"registration_client_uri,omitempty"`
}

func (c *OAuthClient) Authorize(ctx context.Context, options OAuthOptions) (OAuthCredential, error) {
	key, err := c.credentialKey(options)
	if err != nil {
		return OAuthCredential{}, err
	}
	resource, err := NormalizeResourceURL(options.ResourceURL)
	if err != nil {
		return OAuthCredential{}, err
	}
	options.ResourceURL = resource
	metadata, err := c.Discover(ctx, options)
	if err != nil {
		return OAuthCredential{}, err
	}
	listener, redirectURL, err := startOAuthCallbackListener(options.RedirectURL)
	if err != nil {
		return OAuthCredential{}, err
	}
	defer listener.Close()

	existing, getErr := c.store.Get(ctx, key)
	if getErr != nil && !errors.Is(getErr, ErrCredentialsNotFound) {
		return OAuthCredential{}, getErr
	}
	clientID := strings.TrimSpace(options.ClientID)
	clientSecret := options.ClientSecret
	dynamicClient := existing.DynamicClient
	if clientID == "" && dynamicClient != nil {
		clientID = dynamicClient.ClientID
		clientSecret = dynamicClient.ClientSecret
	}
	if clientID == "" {
		dynamicClient, err = c.registerDynamicClient(ctx, metadata.AuthorizationServer, redirectURL)
		if err != nil {
			return OAuthCredential{}, err
		}
		clientID = dynamicClient.ClientID
		clientSecret = dynamicClient.ClientSecret
		existing.DynamicClient = dynamicClient
		if err := c.store.Put(ctx, key, existing); err != nil {
			return OAuthCredential{}, err
		}
	}

	verifier, err := c.randomURLString(32)
	if err != nil {
		return OAuthCredential{}, fmt.Errorf("generate OAuth PKCE verifier: %w", err)
	}
	state, err := c.randomURLString(32)
	if err != nil {
		return OAuthCredential{}, fmt.Errorf("generate OAuth state: %w", err)
	}
	timeout := options.CallbackTimeout
	if timeout <= 0 {
		timeout = defaultOAuthCallbackTimeout
	}
	c.addPending(state, pendingAuthorization{Verifier: verifier, RedirectURL: redirectURL, Credential: key, ExpiresAt: c.now().Add(timeout)})
	defer c.removePending(state)
	callbackResults := make(chan oauthCallbackResult, 1)
	callbackServer := &http.Server{Handler: c.oauthCallbackHandler(callbackResults)}
	serveErrors := make(chan error, 1)
	go func() {
		if serveErr := callbackServer.Serve(listener); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			serveErrors <- serveErr
		}
	}()
	defer callbackServer.Close()

	authorizationURL, err := buildAuthorizationURL(metadata.AuthorizationServer.AuthorizationEndpoint, clientID, redirectURL, resource, normalizedScopes(options.Scopes), state, verifier)
	if err != nil {
		return OAuthCredential{}, err
	}
	if c.openBrowser == nil || c.openBrowser(authorizationURL) != nil {
		fmt.Fprintf(c.output, "Open this URL to authorize MCP server %s:\n%s\n", options.ServerName, authorizationURL)
	}
	waitContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var callback oauthCallbackResult
	select {
	case <-waitContext.Done():
		return OAuthCredential{}, fmt.Errorf("wait for OAuth callback: %w", waitContext.Err())
	case serveErr := <-serveErrors:
		return OAuthCredential{}, fmt.Errorf("serve OAuth callback: %w", serveErr)
	case callback = <-callbackResults:
		if callback.err != nil {
			return OAuthCredential{}, callback.err
		}
	}
	authMethod := selectTokenEndpointAuthMethod(metadata.AuthorizationServer.TokenEndpointAuthMethodsSupported, dynamicClient)
	credential, err := c.exchangeAuthorizationCode(ctx, metadata.AuthorizationServer.TokenEndpoint, clientID, clientSecret, authMethod, callback.code, callback.pending.Verifier, callback.pending.RedirectURL, resource)
	if err != nil {
		return OAuthCredential{}, err
	}
	credential.DynamicClient = dynamicClient
	credential.ResourceMetadataURL = options.ResourceMetadataURL
	if err := c.store.Put(ctx, key, credential); err != nil {
		return OAuthCredential{}, err
	}
	return cloneOAuthCredential(credential), nil
}

func startOAuthCallbackListener(configured string) (net.Listener, string, error) {
	redirectURL := strings.TrimSpace(configured)
	if redirectURL == "" {
		redirectURL = "http://127.0.0.1/oauth/callback"
	}
	normalized, err := normalizeLoopbackRedirect(redirectURL, false)
	if err != nil {
		return nil, "", err
	}
	parsed, err := url.Parse(normalized)
	if err != nil {
		return nil, "", err
	}
	port := parsed.Port()
	if port == "" {
		port = "0"
	}
	listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", port))
	if err != nil {
		return nil, "", fmt.Errorf("listen for OAuth callback: %w", err)
	}
	actualPort := listener.Addr().(*net.TCPAddr).Port
	parsed.Host = fmt.Sprintf("127.0.0.1:%d", actualPort)
	return listener, parsed.String(), nil
}

func (c *OAuthClient) oauthCallbackHandler(results chan<- oauthCallbackResult) http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet {
			http.Error(response, "OAuth callback requires GET", http.StatusMethodNotAllowed)
			return
		}
		state := request.URL.Query().Get("state")
		pending, ok := c.consumePending(state, request.URL.EscapedPath())
		if !ok {
			http.Error(response, "OAuth state is invalid or expired", http.StatusBadRequest)
			return
		}
		result := oauthCallbackResult{pending: pending}
		if oauthError := request.URL.Query().Get("error"); oauthError != "" {
			description := strings.TrimSpace(request.URL.Query().Get("error_description"))
			if description != "" {
				oauthError += ": " + description
			}
			result.err = fmt.Errorf("OAuth authorization failed: %s", oauthError)
		} else {
			result.code = request.URL.Query().Get("code")
			result.issuer = request.URL.Query().Get("iss")
			if result.code == "" {
				result.err = errors.New("OAuth callback omitted the authorization code")
			}
		}
		response.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if result.err != nil {
			response.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(response, "Authorization failed. Return to Eylu for details.\n")
		} else {
			_, _ = io.WriteString(response, "Authorization complete. You can close this window.\n")
		}
		select {
		case results <- result:
		default:
		}
	})
}

func buildAuthorizationURL(endpoint, clientID, redirectURL, resource string, scopes []string, state, verifier string) (string, error) {
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return "", fmt.Errorf("parse OAuth authorization endpoint: %w", err)
	}
	digest := sha256.Sum256([]byte(verifier))
	query := parsed.Query()
	query.Set("response_type", "code")
	query.Set("client_id", clientID)
	query.Set("redirect_uri", redirectURL)
	query.Set("resource", resource)
	query.Set("state", state)
	query.Set("code_challenge", base64RawURL(digest[:]))
	query.Set("code_challenge_method", "S256")
	if len(scopes) > 0 {
		query.Set("scope", strings.Join(scopes, " "))
	}
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}

func (c *OAuthClient) registerDynamicClient(ctx context.Context, metadata AuthorizationServerMetadata, redirectURL string) (*DynamicClientCredentials, error) {
	if metadata.RegistrationEndpoint == "" {
		return nil, errors.New("OAuth client_id is required because the authorization server has no dynamic registration endpoint")
	}
	body, err := json.Marshal(map[string]any{
		"client_name":                "Eylu",
		"redirect_uris":              []string{redirectURL},
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": "none",
	})
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, metadata.RegistrationEndpoint, strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	response, err := c.httpClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("register OAuth client: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated && response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("register OAuth client: %w", oauthHTTPError(response))
	}
	var registration dynamicRegistrationResponse
	if err := json.NewDecoder(io.LimitReader(response.Body, maxOAuthResponseBytes+1)).Decode(&registration); err != nil {
		return nil, fmt.Errorf("decode OAuth client registration: %w", err)
	}
	if strings.TrimSpace(registration.ClientID) == "" {
		return nil, errors.New("OAuth dynamic registration response omitted client_id")
	}
	result := &DynamicClientCredentials{
		ClientID: registration.ClientID, ClientSecret: registration.ClientSecret, TokenEndpointAuthMethod: registration.TokenEndpointAuthMethod,
		RegistrationAccessToken: registration.RegistrationAccessToken, RegistrationClientURI: registration.RegistrationClientURI,
	}
	if registration.ClientIDIssuedAt > 0 {
		result.ClientIDIssuedAt = time.Unix(registration.ClientIDIssuedAt, 0).UTC()
	}
	if registration.ClientSecretExpiresAt > 0 {
		result.ClientSecretExpiresAt = time.Unix(registration.ClientSecretExpiresAt, 0).UTC()
	}
	return result, nil
}

func (c *OAuthClient) exchangeAuthorizationCode(ctx context.Context, endpoint, clientID, clientSecret, authMethod, code, verifier, redirectURL, resource string) (OAuthCredential, error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"client_id":     {clientID},
		"redirect_uri":  {redirectURL},
		"code_verifier": {verifier},
		"resource":      {resource},
	}
	response, err := c.postOAuthForm(ctx, endpoint, form, clientID, clientSecret, authMethod)
	if err != nil {
		return OAuthCredential{}, fmt.Errorf("exchange OAuth authorization code: %w", err)
	}
	return c.credentialFromTokenResponse(response, OAuthCredential{})
}

func (c *OAuthClient) Token(ctx context.Context, options OAuthOptions) (OAuthCredential, error) {
	key, err := c.credentialKey(options)
	if err != nil {
		return OAuthCredential{}, err
	}
	credential, err := c.store.Get(ctx, key)
	if err != nil {
		if errors.Is(err, ErrCredentialsNotFound) {
			return OAuthCredential{}, ErrAuthorizationRequired
		}
		return OAuthCredential{}, err
	}
	if c.usableCredential(credential) {
		return credential, nil
	}
	if credential.RefreshToken == "" {
		return OAuthCredential{}, ErrAuthorizationRequired
	}
	result := c.refreshes.DoChan("refresh:"+key, func() (any, error) {
		return c.refreshUnderLock(ctx, key, options)
	})
	select {
	case <-ctx.Done():
		return OAuthCredential{}, ctx.Err()
	case outcome := <-result:
		if outcome.Err != nil {
			return OAuthCredential{}, outcome.Err
		}
		return cloneOAuthCredential(outcome.Val.(OAuthCredential)), nil
	}
}

func (c *OAuthClient) usableCredential(credential OAuthCredential) bool {
	return credential.AccessToken != "" && (credential.Expiry.IsZero() || credential.Expiry.After(c.now().Add(oauthRefreshSkew)))
}

func (c *OAuthClient) refreshUnderLock(ctx context.Context, key string, options OAuthOptions) (OAuthCredential, error) {
	var refreshed OAuthCredential
	err := c.store.Transaction(ctx, func(transaction *CredentialTransaction) error {
		credential, err := transaction.Get(key)
		if errors.Is(err, ErrCredentialsNotFound) {
			return ErrAuthorizationRequired
		}
		if err != nil {
			return err
		}
		if c.usableCredential(credential) {
			refreshed = credential
			return nil
		}
		if credential.RefreshToken == "" {
			return ErrAuthorizationRequired
		}
		resource, err := NormalizeResourceURL(options.ResourceURL)
		if err != nil {
			return err
		}
		options.ResourceURL = resource
		metadata, err := c.Discover(ctx, options)
		if err != nil {
			return err
		}
		clientID, clientSecret, dynamicAuthMethod, err := oauthClientCredentials(options, credential)
		if err != nil {
			return err
		}
		form := url.Values{
			"grant_type":    {"refresh_token"},
			"refresh_token": {credential.RefreshToken},
			"client_id":     {clientID},
			"resource":      {resource},
		}
		authMethod := selectTokenEndpointAuthMethod(metadata.AuthorizationServer.TokenEndpointAuthMethodsSupported, credential.DynamicClient)
		if dynamicAuthMethod != "" {
			authMethod = dynamicAuthMethod
		}
		response, err := c.postOAuthForm(ctx, metadata.AuthorizationServer.TokenEndpoint, form, clientID, clientSecret, authMethod)
		if err != nil {
			return fmt.Errorf("refresh MCP OAuth token: %w", err)
		}
		refreshed, err = c.credentialFromTokenResponse(response, credential)
		if err != nil {
			return err
		}
		return transaction.Put(key, refreshed)
	})
	return refreshed, err
}

func (c *OAuthClient) AuthorizationHeader(ctx context.Context, options OAuthOptions) (string, error) {
	credential, err := c.Token(ctx, options)
	if err != nil {
		return "", err
	}
	tokenType := strings.TrimSpace(credential.TokenType)
	if tokenType == "" {
		tokenType = "Bearer"
	}
	if len(strings.Fields(tokenType)) != 1 || strings.ContainsAny(tokenType, "\r\n") {
		return "", errors.New("OAuth token_type is invalid")
	}
	return tokenType + " " + credential.AccessToken, nil
}

func (c *OAuthClient) credentialFromTokenResponse(response oauthTokenResponse, previous OAuthCredential) (OAuthCredential, error) {
	if strings.TrimSpace(response.AccessToken) == "" {
		return OAuthCredential{}, errors.New("OAuth token response omitted access_token")
	}
	credential := cloneOAuthCredential(previous)
	credential.AccessToken = response.AccessToken
	if response.RefreshToken != "" {
		credential.RefreshToken = response.RefreshToken
	}
	if response.TokenType != "" {
		credential.TokenType = response.TokenType
	}
	if credential.TokenType == "" {
		credential.TokenType = "Bearer"
	}
	if response.ExpiresIn > 0 {
		credential.Expiry = c.now().Add(time.Duration(response.ExpiresIn) * time.Second).UTC()
	} else {
		credential.Expiry = time.Time{}
	}
	if response.Scope != "" {
		credential.Scope = normalizedScopes([]string{response.Scope})
	}
	return credential, nil
}

func (c *OAuthClient) postOAuthForm(ctx context.Context, endpoint string, form url.Values, clientID, clientSecret, authMethod string) (oauthTokenResponse, error) {
	if clientSecret != "" && authMethod == "client_secret_post" {
		form.Set("client_secret", clientSecret)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return oauthTokenResponse{}, err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "application/json")
	if clientSecret != "" && authMethod != "client_secret_post" {
		request.SetBasicAuth(clientID, clientSecret)
	}
	response, err := c.httpClient.Do(request)
	if err != nil {
		return oauthTokenResponse{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return oauthTokenResponse{}, oauthHTTPError(response)
	}
	var token oauthTokenResponse
	if err := json.NewDecoder(io.LimitReader(response.Body, maxOAuthResponseBytes+1)).Decode(&token); err != nil {
		return oauthTokenResponse{}, fmt.Errorf("decode OAuth token response: %w", err)
	}
	return token, nil
}

func oauthClientCredentials(options OAuthOptions, credential OAuthCredential) (string, string, string, error) {
	if clientID := strings.TrimSpace(options.ClientID); clientID != "" {
		return clientID, options.ClientSecret, "", nil
	}
	if credential.DynamicClient != nil && credential.DynamicClient.ClientID != "" {
		return credential.DynamicClient.ClientID, credential.DynamicClient.ClientSecret, credential.DynamicClient.TokenEndpointAuthMethod, nil
	}
	return "", "", "", ErrAuthorizationRequired
}

func selectTokenEndpointAuthMethod(supported []string, dynamic *DynamicClientCredentials) string {
	if dynamic != nil && dynamic.TokenEndpointAuthMethod != "" {
		return dynamic.TokenEndpointAuthMethod
	}
	if containsString(supported, "client_secret_post") {
		return "client_secret_post"
	}
	return "client_secret_basic"
}

func (c *OAuthClient) Logout(ctx context.Context, options OAuthOptions) error {
	key, err := c.credentialKey(options)
	if err != nil {
		return err
	}
	result := c.refreshes.DoChan("logout:"+key, func() (any, error) {
		return nil, c.logoutUnderLock(ctx, key, options)
	})
	select {
	case <-ctx.Done():
		return ctx.Err()
	case outcome := <-result:
		return outcome.Err
	}
}

func (c *OAuthClient) logoutUnderLock(ctx context.Context, key string, options OAuthOptions) error {
	return c.store.Transaction(ctx, func(transaction *CredentialTransaction) error {
		credential, err := transaction.Get(key)
		if errors.Is(err, ErrCredentialsNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		resource, err := NormalizeResourceURL(options.ResourceURL)
		if err != nil {
			return err
		}
		options.ResourceURL = resource
		metadata, err := c.Discover(ctx, options)
		if err != nil {
			return err
		}
		if endpoint := metadata.AuthorizationServer.RevocationEndpoint; endpoint != "" {
			clientID, clientSecret, _, credentialsErr := oauthClientCredentials(options, credential)
			if credentialsErr != nil {
				return credentialsErr
			}
			for _, token := range []struct {
				value string
				hint  string
			}{{credential.RefreshToken, "refresh_token"}, {credential.AccessToken, "access_token"}} {
				if token.value == "" {
					continue
				}
				if err := c.revokeToken(ctx, endpoint, clientID, clientSecret, token.value, token.hint); err != nil {
					return err
				}
			}
		}
		return transaction.Delete(key)
	})
}

func (c *OAuthClient) revokeToken(ctx context.Context, endpoint, clientID, clientSecret, token, hint string) error {
	form := url.Values{"token": {token}, "token_type_hint": {hint}, "client_id": {clientID}}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if clientSecret != "" {
		request.SetBasicAuth(clientID, clientSecret)
	}
	response, err := c.httpClient.Do(request)
	if err != nil {
		return fmt.Errorf("revoke MCP OAuth %s: %w", hint, err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("revoke MCP OAuth %s: %w", hint, oauthHTTPError(response))
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	return nil
}
