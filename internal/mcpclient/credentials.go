package mcpclient

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const credentialFileVersion = 1

var ErrCredentialsNotFound = errors.New("MCP OAuth credentials not found")

type DynamicClientCredentials struct {
	ClientID                string    `json:"client_id"`
	ClientSecret            string    `json:"client_secret,omitempty"`
	TokenEndpointAuthMethod string    `json:"token_endpoint_auth_method,omitempty"`
	ClientIDIssuedAt        time.Time `json:"client_id_issued_at,omitempty"`
	ClientSecretExpiresAt   time.Time `json:"client_secret_expires_at,omitempty"`
	RegistrationAccessToken string    `json:"registration_access_token,omitempty"`
	RegistrationClientURI   string    `json:"registration_client_uri,omitempty"`
}

type OAuthCredential struct {
	AccessToken         string                    `json:"access_token,omitempty"`
	RefreshToken        string                    `json:"refresh_token,omitempty"`
	TokenType           string                    `json:"token_type,omitempty"`
	Expiry              time.Time                 `json:"expiry,omitempty"`
	Scope               []string                  `json:"scope,omitempty"`
	ResourceMetadataURL string                    `json:"resource_metadata_url,omitempty"`
	DynamicClient       *DynamicClientCredentials `json:"dynamic_client,omitempty"`
}

type CredentialStore struct {
	path string
	mu   sync.Mutex
}

// CredentialTransaction provides read/write access while CredentialStore holds
// its cross-process lock. Values are committed atomically when the callback
// passed to Transaction returns nil.
type CredentialTransaction struct {
	file  credentialFile
	dirty bool
}

type credentialFile struct {
	Version     int                        `json:"version"`
	Credentials map[string]OAuthCredential `json:"credentials"`
}

func DefaultCredentialStore() (*CredentialStore, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("resolve home directory for MCP credentials: %w", err)
	}
	return NewCredentialStore(filepath.Join(home, ".eylu", "mcp_credentials.json")), nil
}

func NewCredentialStore(path string) *CredentialStore {
	return &CredentialStore{path: filepath.Clean(path)}
}

func (s *CredentialStore) Path() string {
	if s == nil {
		return ""
	}
	return s.path
}

func CredentialKey(serverName, resourceURL, authHash string) (string, error) {
	serverName = strings.TrimSpace(serverName)
	if serverName == "" {
		return "", errors.New("MCP OAuth server name is required")
	}
	resource, err := NormalizeResourceURL(resourceURL)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(authHash) == "" {
		return "", errors.New("MCP OAuth auth hash is required")
	}
	digest := sha256.Sum256([]byte(lengthPrefixed(serverName, resource, authHash)))
	return hex.EncodeToString(digest[:]), nil
}

func NormalizeResourceURL(rawURL string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return "", fmt.Errorf("parse MCP resource URL: %w", err)
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	if parsed.Scheme != "https" && !(parsed.Scheme == "http" && isLoopbackHostname(parsed.Hostname())) {
		return "", errors.New("MCP resource URL must use HTTPS or loopback HTTP")
	}
	if parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" {
		return "", errors.New("MCP resource URL must contain a host and omit userinfo and fragments")
	}
	hostname := strings.ToLower(parsed.Hostname())
	port := parsed.Port()
	if parsed.Scheme == "https" && port == "443" || parsed.Scheme == "http" && port == "80" {
		port = ""
	}
	parsed.Host = hostname
	if strings.Contains(hostname, ":") {
		parsed.Host = "[" + hostname + "]"
	}
	if port != "" {
		parsed.Host += ":" + port
	}
	if parsed.Path == "/" {
		parsed.Path = ""
	}
	parsed.RawQuery = parsed.Query().Encode()
	parsed.ForceQuery = false
	return parsed.String(), nil
}

func lengthPrefixed(values ...string) string {
	var result strings.Builder
	for _, value := range values {
		fmt.Fprintf(&result, "%d:%s", len(value), value)
	}
	return result.String()
}

func normalizedScopes(scopes []string) []string {
	seen := make(map[string]struct{}, len(scopes))
	result := make([]string, 0, len(scopes))
	for _, scope := range scopes {
		for _, value := range strings.Fields(scope) {
			if _, exists := seen[value]; exists {
				continue
			}
			seen[value] = struct{}{}
			result = append(result, value)
		}
	}
	sort.Strings(result)
	return result
}

func (s *CredentialStore) Get(ctx context.Context, key string) (OAuthCredential, error) {
	var result OAuthCredential
	err := s.Transaction(ctx, func(transaction *CredentialTransaction) error {
		var err error
		result, err = transaction.Get(key)
		return err
	})
	return result, err
}

func (s *CredentialStore) Put(ctx context.Context, key string, credential OAuthCredential) error {
	return s.Transaction(ctx, func(transaction *CredentialTransaction) error {
		return transaction.Put(key, credential)
	})
}

func (s *CredentialStore) Delete(ctx context.Context, key string) error {
	return s.Transaction(ctx, func(transaction *CredentialTransaction) error {
		return transaction.Delete(key)
	})
}

func (s *CredentialStore) Transaction(ctx context.Context, action func(*CredentialTransaction) error) error {
	if s == nil || strings.TrimSpace(s.path) == "" {
		return errors.New("MCP credential store path is required")
	}
	if action == nil {
		return errors.New("MCP credential transaction callback is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.withLock(ctx, func() error {
		file, err := s.readUnlocked()
		if err != nil {
			return err
		}
		transaction := &CredentialTransaction{file: file}
		if err := action(transaction); err != nil {
			return err
		}
		if !transaction.dirty {
			return nil
		}
		return s.writeUnlocked(transaction.file)
	})
}

func (t *CredentialTransaction) Get(key string) (OAuthCredential, error) {
	if t == nil {
		return OAuthCredential{}, errors.New("MCP credential transaction is required")
	}
	if strings.TrimSpace(key) == "" {
		return OAuthCredential{}, errors.New("MCP credential key is required")
	}
	credential, exists := t.file.Credentials[key]
	if !exists {
		return OAuthCredential{}, ErrCredentialsNotFound
	}
	return cloneOAuthCredential(credential), nil
}

func (t *CredentialTransaction) Put(key string, credential OAuthCredential) error {
	if t == nil {
		return errors.New("MCP credential transaction is required")
	}
	if strings.TrimSpace(key) == "" {
		return errors.New("MCP credential key is required")
	}
	if t.file.Credentials == nil {
		t.file.Credentials = make(map[string]OAuthCredential)
	}
	t.file.Credentials[key] = cloneOAuthCredential(credential)
	t.dirty = true
	return nil
}

func (t *CredentialTransaction) Delete(key string) error {
	if t == nil {
		return errors.New("MCP credential transaction is required")
	}
	if strings.TrimSpace(key) == "" {
		return errors.New("MCP credential key is required")
	}
	if _, exists := t.file.Credentials[key]; exists {
		delete(t.file.Credentials, key)
		t.dirty = true
	}
	return nil
}

func (s *CredentialStore) withLock(ctx context.Context, action func() error) error {
	directory := filepath.Dir(s.path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("create MCP credential directory: %w", err)
	}
	if err := secureCredentialDirectory(directory); err != nil {
		return fmt.Errorf("secure MCP credential directory: %w", err)
	}
	unlock, err := acquireCredentialFileLock(ctx, s.path+".lock")
	if err != nil {
		return fmt.Errorf("lock MCP credentials: %w", err)
	}
	defer unlock()
	return action()
}

func (s *CredentialStore) readUnlocked() (credentialFile, error) {
	file := credentialFile{Version: credentialFileVersion, Credentials: make(map[string]OAuthCredential)}
	info, err := os.Lstat(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return file, nil
	}
	if err != nil {
		return credentialFile{}, fmt.Errorf("inspect MCP credentials: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return credentialFile{}, errors.New("MCP credential path must be a regular file")
	}
	encoded, err := os.ReadFile(s.path)
	if err != nil {
		return credentialFile{}, fmt.Errorf("read MCP credentials: %w", err)
	}
	if len(encoded) == 0 {
		return file, nil
	}
	decoderErr := json.Unmarshal(encoded, &file)
	if decoderErr != nil {
		return credentialFile{}, fmt.Errorf("decode MCP credentials: %w", decoderErr)
	}
	if file.Version != credentialFileVersion {
		return credentialFile{}, fmt.Errorf("unsupported MCP credential file version %d", file.Version)
	}
	if file.Credentials == nil {
		file.Credentials = make(map[string]OAuthCredential)
	}
	return file, nil
}

func (s *CredentialStore) writeUnlocked(file credentialFile) error {
	file.Version = credentialFileVersion
	if file.Credentials == nil {
		file.Credentials = make(map[string]OAuthCredential)
	}
	encoded, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return fmt.Errorf("encode MCP credentials: %w", err)
	}
	encoded = append(encoded, '\n')
	directory := filepath.Dir(s.path)
	temporary, err := os.CreateTemp(directory, "."+filepath.Base(s.path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temporary MCP credential file: %w", err)
	}
	temporaryPath := temporary.Name()
	removeTemporary := true
	defer func() {
		if removeTemporary {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return fmt.Errorf("set temporary MCP credential permissions: %w", err)
	}
	if err := secureCredentialFile(temporaryPath); err != nil {
		temporary.Close()
		return fmt.Errorf("secure temporary MCP credential file: %w", err)
	}
	if _, err := temporary.Write(encoded); err != nil {
		temporary.Close()
		return fmt.Errorf("write temporary MCP credentials: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return fmt.Errorf("sync temporary MCP credentials: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close temporary MCP credentials: %w", err)
	}
	if err := atomicReplaceCredentialFile(temporaryPath, s.path); err != nil {
		return fmt.Errorf("replace MCP credentials: %w", err)
	}
	removeTemporary = false
	if err := secureCredentialFile(s.path); err != nil {
		return fmt.Errorf("secure MCP credential file: %w", err)
	}
	if err := syncCredentialDirectory(directory); err != nil {
		return fmt.Errorf("sync MCP credential directory: %w", err)
	}
	return nil
}

func cloneOAuthCredential(value OAuthCredential) OAuthCredential {
	result := value
	result.Scope = append([]string(nil), value.Scope...)
	if value.DynamicClient != nil {
		client := *value.DynamicClient
		result.DynamicClient = &client
	}
	return result
}
