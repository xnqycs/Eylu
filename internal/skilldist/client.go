package skilldist

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"golang.org/x/mod/semver"

	"Eylu/internal/config"
)

const (
	maxIndexBytes   = 2 << 20
	maxIndexEntries = 2000
	maxPackageBytes = 8 << 20
)

type Registry struct {
	Name   string
	Config config.SkillRegistryConfig
	client *http.Client
}

func NewRegistry(name string, registryConfig config.SkillRegistryConfig, client *http.Client) *Registry {
	if client == nil {
		timeout := time.Duration(registryConfig.TimeoutSeconds) * time.Second
		if timeout <= 0 {
			timeout = 30 * time.Second
		}
		client = &http.Client{Timeout: timeout, CheckRedirect: safeRedirect}
	}
	return &Registry{Name: name, Config: registryConfig, client: client}
}

func (r *Registry) Fetch(ctx context.Context) (Index, error) {
	data, err := r.fetch(ctx, r.Config.IndexURL, maxIndexBytes)
	if err != nil {
		return Index{}, fmt.Errorf("fetch Skill registry %s: %w", r.Name, err)
	}
	var index Index
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&index); err != nil {
		return Index{}, fmt.Errorf("decode Skill registry %s: %w", r.Name, err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return Index{}, fmt.Errorf("decode Skill registry %s: trailing JSON content", r.Name)
	}
	if index.Version != IndexVersion {
		return Index{}, fmt.Errorf("skill registry %s uses unsupported index version %d", r.Name, index.Version)
	}
	if len(index.Skills) > maxIndexEntries {
		return Index{}, fmt.Errorf("skill registry %s exceeds %d entries", r.Name, maxIndexEntries)
	}
	seen := make(map[string]bool, len(index.Skills))
	for entryIndex := range index.Skills {
		entry := &index.Skills[entryIndex]
		entry.Version = normalizeVersion(entry.Version)
		packageURL, err := r.resolvePackageURL(entry.PackageURL)
		if err != nil {
			return Index{}, fmt.Errorf("skill %s package URL: %w", entry.Name, err)
		}
		entry.PackageURL = packageURL
		if err := validateEntry(*entry, r.Config.PublicKeys); err != nil {
			return Index{}, err
		}
		key := entry.Name + "@" + entry.Version
		if seen[key] {
			return Index{}, fmt.Errorf("skill registry %s contains duplicate %s", r.Name, key)
		}
		seen[key] = true
	}
	sort.Slice(index.Skills, func(i, j int) bool {
		if index.Skills[i].Name != index.Skills[j].Name {
			return index.Skills[i].Name < index.Skills[j].Name
		}
		return semver.Compare(index.Skills[i].Version, index.Skills[j].Version) > 0
	})
	return index, nil
}

func (r *Registry) Select(ctx context.Context, name, version string) (Entry, error) {
	index, err := r.Fetch(ctx)
	if err != nil {
		return Entry{}, err
	}
	if version != "" {
		version = normalizeVersion(version)
	}
	for _, entry := range index.Skills {
		if entry.Name == name && (version == "" || entry.Version == version) {
			return entry, nil
		}
	}
	return Entry{}, fmt.Errorf("skill %s version %s was not found in registry %s", name, version, r.Name)
}

func (r *Registry) Download(ctx context.Context, entry Entry) ([]byte, error) {
	data, err := r.fetch(ctx, entry.PackageURL, maxPackageBytes)
	if err != nil {
		return nil, fmt.Errorf("download Skill %s: %w", entry.Name, err)
	}
	return data, nil
}

func (r *Registry) fetch(ctx context.Context, target string, limit int64) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/json, application/zip")
	if r.Config.TokenEnvironment != "" {
		if token := os.Getenv(r.Config.TokenEnvironment); token != "" {
			request.Header.Set("Authorization", "Bearer "+token)
		}
	}
	response, err := r.client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("http %d", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("response exceeds %d bytes", limit)
	}
	return data, nil
}

func (r *Registry) resolvePackageURL(raw string) (string, error) {
	base, err := url.Parse(r.Config.IndexURL)
	if err != nil {
		return "", err
	}
	reference, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	resolved := base.ResolveReference(reference)
	if resolved.Scheme != "https" && !(resolved.Scheme == "http" && loopback(resolved.Hostname())) {
		return "", errors.New("package URL must use HTTPS or loopback HTTP")
	}
	return resolved.String(), nil
}

func safeRedirect(request *http.Request, via []*http.Request) error {
	if len(via) >= 3 {
		return errors.New("redirect limit exceeded")
	}
	if len(via) > 0 && (request.URL.Scheme != via[0].URL.Scheme || request.URL.Host != via[0].URL.Host) {
		return errors.New("cross-origin redirect rejected")
	}
	return nil
}

func loopback(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}
