package mcpclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

type ProtectedResourceMetadata struct {
	Resource             string   `json:"resource"`
	AuthorizationServers []string `json:"authorization_servers,omitempty"`
	ScopesSupported      []string `json:"scopes_supported,omitempty"`
}

type AuthorizationServerMetadata struct {
	Issuer                            string   `json:"issuer"`
	AuthorizationEndpoint             string   `json:"authorization_endpoint"`
	TokenEndpoint                     string   `json:"token_endpoint"`
	RegistrationEndpoint              string   `json:"registration_endpoint,omitempty"`
	RevocationEndpoint                string   `json:"revocation_endpoint,omitempty"`
	CodeChallengeMethodsSupported     []string `json:"code_challenge_methods_supported,omitempty"`
	TokenEndpointAuthMethodsSupported []string `json:"token_endpoint_auth_methods_supported,omitempty"`
}

type OAuthMetadata struct {
	ProtectedResource   ProtectedResourceMetadata
	AuthorizationServer AuthorizationServerMetadata
}

func ProtectedResourceMetadataURL(resourceURL string) (string, error) {
	normalized, err := NormalizeResourceURL(resourceURL)
	if err != nil {
		return "", err
	}
	parsed, err := url.Parse(normalized)
	if err != nil {
		return "", err
	}
	parsed.Path = "/.well-known/oauth-protected-resource" + parsed.Path
	parsed.RawPath = ""
	return parsed.String(), nil
}

func AuthorizationServerMetadataURL(issuer string) (string, error) {
	normalized, err := normalizeIssuer(issuer)
	if err != nil {
		return "", err
	}
	parsed, err := url.Parse(normalized)
	if err != nil {
		return "", err
	}
	parsed.Path = "/.well-known/oauth-authorization-server" + parsed.Path
	parsed.RawPath = ""
	return parsed.String(), nil
}

func (c *OAuthClient) Discover(ctx context.Context, options OAuthOptions) (OAuthMetadata, error) {
	if c == nil || c.httpClient == nil {
		return OAuthMetadata{}, errors.New("MCP OAuth HTTP client is required")
	}
	resource, err := NormalizeResourceURL(options.ResourceURL)
	if err != nil {
		return OAuthMetadata{}, err
	}
	metadataURL := strings.TrimSpace(options.ResourceMetadataURL)
	if metadataURL == "" {
		metadataURL, err = ProtectedResourceMetadataURL(resource)
		if err != nil {
			return OAuthMetadata{}, err
		}
	} else if err := validateEndpointString(metadataURL, "protected resource metadata URL"); err != nil {
		return OAuthMetadata{}, err
	}
	var protected ProtectedResourceMetadata
	if err := c.getOAuthJSON(ctx, metadataURL, &protected); err != nil {
		return OAuthMetadata{}, fmt.Errorf("discover protected resource metadata: %w", err)
	}
	if protected.Resource != resource {
		return OAuthMetadata{}, errors.New("protected resource metadata resource does not match the MCP resource URL")
	}
	issuer := strings.TrimSpace(options.Issuer)
	if issuer == "" {
		if len(protected.AuthorizationServers) == 0 {
			return OAuthMetadata{}, errors.New("protected resource metadata has no authorization server")
		}
		issuer = protected.AuthorizationServers[0]
	}
	issuer, err = normalizeIssuer(issuer)
	if err != nil {
		return OAuthMetadata{}, err
	}
	if len(protected.AuthorizationServers) > 0 && !containsNormalizedIssuer(protected.AuthorizationServers, issuer) {
		return OAuthMetadata{}, errors.New("configured OAuth issuer is not advertised by the protected resource")
	}
	authorizationMetadataURL, err := AuthorizationServerMetadataURL(issuer)
	if err != nil {
		return OAuthMetadata{}, err
	}
	var authorization AuthorizationServerMetadata
	if err := c.getOAuthJSON(ctx, authorizationMetadataURL, &authorization); err != nil {
		return OAuthMetadata{}, fmt.Errorf("discover authorization server metadata: %w", err)
	}
	metadataIssuer, err := normalizeIssuer(authorization.Issuer)
	if err != nil || metadataIssuer != issuer {
		return OAuthMetadata{}, errors.New("authorization server metadata issuer does not match the requested issuer")
	}
	for label, endpoint := range map[string]string{
		"authorization endpoint": authorization.AuthorizationEndpoint,
		"token endpoint":         authorization.TokenEndpoint,
	} {
		if strings.TrimSpace(endpoint) == "" {
			return OAuthMetadata{}, fmt.Errorf("authorization server metadata is missing %s", label)
		}
		if err := validateEndpointString(endpoint, label); err != nil {
			return OAuthMetadata{}, err
		}
	}
	for label, endpoint := range map[string]string{
		"registration endpoint": authorization.RegistrationEndpoint,
		"revocation endpoint":   authorization.RevocationEndpoint,
	} {
		if endpoint != "" {
			if err := validateEndpointString(endpoint, label); err != nil {
				return OAuthMetadata{}, err
			}
		}
	}
	if len(authorization.CodeChallengeMethodsSupported) > 0 && !containsString(authorization.CodeChallengeMethodsSupported, "S256") {
		return OAuthMetadata{}, errors.New("authorization server does not advertise PKCE S256 support")
	}
	return OAuthMetadata{ProtectedResource: protected, AuthorizationServer: authorization}, nil
}

func validateEndpointString(rawURL, label string) error {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return fmt.Errorf("parse %s: %w", label, err)
	}
	if err := validateOAuthEndpoint(parsed, label); err != nil {
		return err
	}
	if parsed.User != nil || parsed.Fragment != "" {
		return fmt.Errorf("%s must omit userinfo and fragments", label)
	}
	return nil
}

func containsNormalizedIssuer(values []string, issuer string) bool {
	for _, value := range values {
		normalized, err := normalizeIssuer(value)
		if err == nil && normalized == issuer {
			return true
		}
	}
	return false
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func (c *OAuthClient) getOAuthJSON(ctx context.Context, endpoint string, destination any) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "application/json")
	response, err := c.httpClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return oauthHTTPError(response)
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, maxOAuthResponseBytes+1))
	if err := decoder.Decode(destination); err != nil {
		return fmt.Errorf("decode OAuth JSON response: %w", err)
	}
	return nil
}

func oauthHTTPError(response *http.Response) error {
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	return fmt.Errorf("OAuth endpoint returned HTTP %d", response.StatusCode)
}

func ParseResourceMetadataURL(header string) (string, bool) {
	const parameter = "resource_metadata"
	for index := 0; index < len(header); {
		for index < len(header) && (header[index] == ' ' || header[index] == '\t' || header[index] == ',') {
			index++
		}
		start := index
		for index < len(header) && isAuthTokenByte(header[index]) {
			index++
		}
		if index == start {
			index++
			continue
		}
		name := strings.ToLower(header[start:index])
		for index < len(header) && (header[index] == ' ' || header[index] == '\t') {
			index++
		}
		if index >= len(header) || header[index] != '=' {
			continue
		}
		index++
		for index < len(header) && (header[index] == ' ' || header[index] == '\t') {
			index++
		}
		value := ""
		if index < len(header) && header[index] == '"' {
			index++
			var builder strings.Builder
			for index < len(header) && header[index] != '"' {
				if header[index] == '\\' && index+1 < len(header) {
					index++
				}
				builder.WriteByte(header[index])
				index++
			}
			if index < len(header) {
				index++
			}
			value = builder.String()
		} else {
			start = index
			for index < len(header) && header[index] != ',' && header[index] != ' ' && header[index] != '\t' {
				index++
			}
			value = header[start:index]
		}
		if name == parameter && value != "" {
			return value, true
		}
	}
	return "", false
}

func isAuthTokenByte(value byte) bool {
	return value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z' || value >= '0' && value <= '9' || strings.ContainsRune("!#$%&'*+-.^_`|~", rune(value))
}
