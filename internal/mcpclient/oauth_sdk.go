package mcpclient

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"

	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"golang.org/x/oauth2"
)

// SDKHandler constructs the OAuth handler consumed by
// mcp.StreamableClientTransport. Its SDK persistence hooks restore credentials
// before the first request and save both authorization and refresh results.
func (c *OAuthClient) SDKHandler(ctx context.Context, options OAuthOptions) (sdkauth.OAuthHandler, error) {
	key, err := c.credentialKey(options)
	if err != nil {
		return nil, err
	}
	redirectURL, err := reserveOAuthRedirectURL(options.RedirectURL)
	if err != nil {
		return nil, err
	}
	credential, credentialErr := c.store.Get(ctx, key)
	if credentialErr != nil && !errors.Is(credentialErr, ErrCredentialsNotFound) {
		return nil, credentialErr
	}
	runtimeOptions := &sdkOAuthRuntimeOptions{value: options}
	if options.ResourceMetadataURL == "" && credential.ResourceMetadataURL != "" {
		runtimeOptions.value.ResourceMetadataURL = credential.ResourceMetadataURL
	}
	config := &sdkauth.AuthorizationCodeHandlerConfig{
		RedirectURL:              redirectURL,
		AuthorizationCodeFetcher: c.sdkAuthorizationCodeFetcher(runtimeOptions.snapshot(), key, redirectURL),
		RequestRefreshToken:      true,
		Client:                   c.httpClient,
	}
	clientCredentials := sdkClientCredentials(options, credential)
	if clientCredentials != nil {
		config.PreregisteredClient = clientCredentials
	} else {
		config.DynamicClientRegistrationConfig = &sdkauth.DynamicClientRegistrationConfig{
			Metadata: &oauthex.ClientRegistrationMetadata{
				RedirectURIs:            []string{redirectURL},
				TokenEndpointAuthMethod: "none",
				GrantTypes:              []string{"authorization_code", "refresh_token"},
				ResponseTypes:           []string{"code"},
				ClientName:              "Eylu",
				Scope:                   strings.Join(normalizedScopes(options.Scopes), " "),
			},
		}
	}
	if credentialErr == nil && credential.AccessToken != "" {
		config.InitialTokenSource = &sdkCredentialTokenSource{ctx: context.Background(), client: c, options: runtimeOptions}
	}
	config.NewTokenSource = func(refreshContext context.Context, oauthConfig *oauth2.Config, token *oauth2.Token) (oauth2.TokenSource, error) {
		if oauthConfig == nil || token == nil {
			return nil, errors.New("SDK OAuth persistence hook received an incomplete token source")
		}
		stored := oauthCredentialFromSDKToken(token)
		currentOptions := runtimeOptions.snapshot()
		stored.ResourceMetadataURL = currentOptions.ResourceMetadataURL
		if strings.TrimSpace(options.ClientID) == "" {
			stored.DynamicClient = &DynamicClientCredentials{
				ClientID: oauthConfig.ClientID, ClientSecret: oauthConfig.ClientSecret,
				TokenEndpointAuthMethod: sdkTokenEndpointAuthMethod(oauthConfig.Endpoint.AuthStyle, oauthConfig.ClientSecret),
			}
		}
		if err := c.store.Transaction(refreshContext, func(transaction *CredentialTransaction) error {
			return transaction.Put(key, stored)
		}); err != nil {
			return nil, fmt.Errorf("persist SDK OAuth token: %w", err)
		}
		return &sdkCredentialTokenSource{ctx: refreshContext, client: c, options: runtimeOptions}, nil
	}
	handler, err := sdkauth.NewAuthorizationCodeHandler(config)
	if err != nil {
		return nil, err
	}
	return &sdkOAuthHandler{AuthorizationCodeHandler: handler, options: runtimeOptions}, nil
}

func sdkTokenEndpointAuthMethod(style oauth2.AuthStyle, clientSecret string) string {
	if clientSecret == "" {
		return "none"
	}
	if style == oauth2.AuthStyleInParams {
		return "client_secret_post"
	}
	return "client_secret_basic"
}

type sdkCredentialTokenSource struct {
	ctx     context.Context
	client  *OAuthClient
	options *sdkOAuthRuntimeOptions
}

func (s *sdkCredentialTokenSource) Token() (*oauth2.Token, error) {
	if s == nil || s.client == nil {
		return nil, errors.New("SDK MCP OAuth token source is unavailable")
	}
	ctx := s.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	credential, err := s.client.Token(ctx, s.options.snapshot())
	if err != nil {
		return nil, err
	}
	return sdkTokenFromOAuthCredential(credential), nil
}

type sdkOAuthRuntimeOptions struct {
	mu    sync.RWMutex
	value OAuthOptions
}

func (o *sdkOAuthRuntimeOptions) snapshot() OAuthOptions {
	o.mu.RLock()
	defer o.mu.RUnlock()
	result := o.value
	result.Scopes = append([]string(nil), o.value.Scopes...)
	return result
}

func (o *sdkOAuthRuntimeOptions) setResourceMetadataURL(value string) {
	o.mu.Lock()
	o.value.ResourceMetadataURL = value
	o.mu.Unlock()
}

type sdkOAuthHandler struct {
	*sdkauth.AuthorizationCodeHandler
	options *sdkOAuthRuntimeOptions
}

func (h *sdkOAuthHandler) Authorize(ctx context.Context, request *http.Request, response *http.Response) error {
	if response != nil {
		for _, header := range response.Header.Values("WWW-Authenticate") {
			if metadataURL, ok := ParseResourceMetadataURL(header); ok {
				h.options.setResourceMetadataURL(metadataURL)
				break
			}
		}
	}
	return h.AuthorizationCodeHandler.Authorize(ctx, request, response)
}

func sdkClientCredentials(options OAuthOptions, credential OAuthCredential) *oauthex.ClientCredentials {
	clientID := strings.TrimSpace(options.ClientID)
	clientSecret := options.ClientSecret
	if clientID == "" && credential.DynamicClient != nil {
		clientID = credential.DynamicClient.ClientID
		clientSecret = credential.DynamicClient.ClientSecret
	}
	if clientID == "" {
		return nil
	}
	result := &oauthex.ClientCredentials{ClientID: clientID, Issuer: strings.TrimSpace(options.Issuer)}
	if clientSecret != "" {
		result.ClientSecretAuth = &oauthex.ClientSecretAuth{ClientSecret: clientSecret}
	}
	return result
}

func reserveOAuthRedirectURL(configured string) (string, error) {
	listener, redirectURL, err := startOAuthCallbackListener(configured)
	if err != nil {
		return "", err
	}
	if err := listener.Close(); err != nil {
		return "", fmt.Errorf("release OAuth callback port: %w", err)
	}
	return redirectURL, nil
}

func (c *OAuthClient) sdkAuthorizationCodeFetcher(options OAuthOptions, key, redirectURL string) sdkauth.AuthorizationCodeFetcher {
	return func(ctx context.Context, arguments *sdkauth.AuthorizationArgs) (*sdkauth.AuthorizationResult, error) {
		if arguments == nil || strings.TrimSpace(arguments.URL) == "" {
			return nil, errors.New("SDK OAuth authorization URL is required")
		}
		authorizationURL, err := url.Parse(arguments.URL)
		if err != nil {
			return nil, fmt.Errorf("parse SDK OAuth authorization URL: %w", err)
		}
		query := authorizationURL.Query()
		state := query.Get("state")
		if state == "" {
			return nil, errors.New("SDK OAuth authorization URL omitted state")
		}
		configuredScopes := normalizedScopes(options.Scopes)
		if len(configuredScopes) > 0 {
			query.Set("scope", strings.Join(normalizedScopes(append(strings.Fields(query.Get("scope")), configuredScopes...)), " "))
			authorizationURL.RawQuery = query.Encode()
		}
		listener, actualRedirectURL, err := startOAuthCallbackListener(redirectURL)
		if err != nil {
			return nil, err
		}
		defer listener.Close()
		if actualRedirectURL != redirectURL {
			return nil, errors.New("OAuth callback listener changed the SDK redirect URL")
		}
		timeout := options.CallbackTimeout
		if timeout <= 0 {
			timeout = defaultOAuthCallbackTimeout
		}
		c.addPending(state, pendingAuthorization{Verifier: "sdk-managed", RedirectURL: redirectURL, Credential: key, ExpiresAt: c.now().Add(timeout)})
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
		if c.openBrowser == nil || c.openBrowser(authorizationURL.String()) != nil {
			fmt.Fprintf(c.output, "Open this URL to authorize MCP server %s:\n%s\n", options.ServerName, authorizationURL.String())
		}
		waitContext, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		select {
		case <-waitContext.Done():
			return nil, fmt.Errorf("wait for SDK OAuth callback: %w", waitContext.Err())
		case serveErr := <-serveErrors:
			return nil, fmt.Errorf("serve SDK OAuth callback: %w", serveErr)
		case callback := <-callbackResults:
			if callback.err != nil {
				return nil, callback.err
			}
			return &sdkauth.AuthorizationResult{Code: callback.code, State: state, Iss: callback.issuer}, nil
		}
	}
}

func oauthCredentialFromSDKToken(token *oauth2.Token) OAuthCredential {
	credential := OAuthCredential{
		AccessToken: token.AccessToken, RefreshToken: token.RefreshToken, TokenType: token.TokenType, Expiry: token.Expiry.UTC(),
	}
	switch scope := token.Extra("scope").(type) {
	case string:
		credential.Scope = normalizedScopes([]string{scope})
	case []string:
		credential.Scope = normalizedScopes(scope)
	case []any:
		for _, value := range scope {
			if stringValue, ok := value.(string); ok {
				credential.Scope = append(credential.Scope, stringValue)
			}
		}
		credential.Scope = normalizedScopes(credential.Scope)
	}
	return credential
}

func sdkTokenFromOAuthCredential(credential OAuthCredential) *oauth2.Token {
	token := &oauth2.Token{
		AccessToken: credential.AccessToken, RefreshToken: credential.RefreshToken, TokenType: credential.TokenType, Expiry: credential.Expiry,
	}
	if len(credential.Scope) > 0 {
		token = token.WithExtra(map[string]any{"scope": strings.Join(credential.Scope, " ")})
	}
	return token
}
