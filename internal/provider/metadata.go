package provider

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"Eylu/internal/config"
)

type LimitSource string

const (
	LimitSourceUnknown         LimitSource = "unknown"
	LimitSourceUserCap         LimitSource = "user_cap"
	LimitSourceOverflow        LimitSource = "overflow"
	LimitSourceOpenAIExtension LimitSource = "endpoint_openai_extension"
	LimitSourceOpenRouter      LimitSource = "endpoint_openrouter"
	LimitSourceOllamaPS        LimitSource = "endpoint_ollama_ps"
	LimitSourceOllamaShow      LimitSource = "endpoint_ollama_show"
	LimitSourceLlamaCPP        LimitSource = "endpoint_llamacpp"
	LimitSourceModelsDev       LimitSource = "models_dev"
	LimitSourceFallback        LimitSource = "fallback"
)

type ModelLimits struct {
	ContextWindow   int         `json:"context_window,omitempty"`
	MaxOutputTokens int         `json:"max_output_tokens,omitempty"`
	Source          LimitSource `json:"source"`
	ObservedAt      time.Time   `json:"observed_at,omitzero"`
	Cached          bool        `json:"cached,omitempty"`
	Assumed         bool        `json:"assumed,omitempty"`
	Degradations    int         `json:"degradations,omitempty"`
}

func (s Snapshot) WithLimits(limits ModelLimits) Snapshot {
	s.Limits = limits
	s.EffectiveContextWindow = limits.ContextWindow
	if configured := s.Config.ContextWindow; configured > 0 {
		s.EffectiveContextWindow = configured
	}
	return s
}

func (s Snapshot) ContextWindowLimit() int {
	if s.EffectiveContextWindow > 0 {
		return s.EffectiveContextWindow
	}
	return s.Config.ContextWindow
}

type cacheEntry struct {
	Limits     ModelLimits `json:"limits"`
	ExpiresAt  time.Time   `json:"expires_at"`
	StaleUntil time.Time   `json:"stale_until"`
	Negative   bool        `json:"negative,omitempty"`
}

type metadataCacheFile struct {
	Version int                   `json:"version"`
	Entries map[string]cacheEntry `json:"entries"`
}

type catalogProvider struct {
	ID     string                  `json:"id"`
	Models map[string]catalogModel `json:"models"`
}

type catalogModel struct {
	ID    string `json:"id"`
	Limit struct {
		Context int `json:"context"`
		Output  int `json:"output"`
	} `json:"limit"`
}

type LimitResolver struct {
	config    config.ModelMetadataConfig
	cachePath string
	client    *http.Client
	now       func() time.Time
	mu        sync.Mutex
	loaded    bool
	entries   map[string]cacheEntry
	inflight  map[string]chan struct{}
	catalogMu sync.Mutex
	catalog   map[string]catalogProvider
	catalogAt time.Time
}

func DefaultMetadataCachePath() string {
	if state := strings.TrimSpace(os.Getenv("EYLU_STATE_DIR")); state != "" {
		return filepath.Join(state, "model-metadata.json")
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join(".eylu", "state", "model-metadata.json")
	}
	return filepath.Join(home, ".eylu", "state", "model-metadata.json")
}

func NewLimitResolver(metadata config.ModelMetadataConfig, cachePath string, client *http.Client) *LimitResolver {
	if cachePath == "" {
		cachePath = DefaultMetadataCachePath()
	}
	if client == nil {
		client = http.DefaultClient
	}
	return &LimitResolver{config: metadata, cachePath: cachePath, client: client, now: time.Now, entries: make(map[string]cacheEntry), inflight: make(map[string]chan struct{})}
}

func (r *LimitResolver) Resolve(ctx context.Context, snapshot Snapshot, apiKey string) (Snapshot, error) {
	key := metadataKey(snapshot.Config)
	for {
		now := r.now().UTC()
		r.mu.Lock()
		r.loadCacheLocked()
		if entry, ok := r.entries[key]; ok && now.Before(entry.ExpiresAt) {
			limits := entry.Limits
			limits.Cached = true
			r.mu.Unlock()
			return snapshot.WithLimits(limits), nil
		}
		if wait, ok := r.inflight[key]; ok {
			r.mu.Unlock()
			select {
			case <-ctx.Done():
				return Snapshot{}, ctx.Err()
			case <-wait:
				continue
			}
		}
		wait := make(chan struct{})
		r.inflight[key] = wait
		stale, hasStale := r.entries[key]
		r.mu.Unlock()

		limits, found := ModelLimits{}, false
		if r.config.Enabled {
			limits, found = r.resolveRemote(ctx, snapshot, apiKey)
		}
		if ctx.Err() != nil {
			r.finishInflight(key, wait)
			return Snapshot{}, ctx.Err()
		}
		now = r.now().UTC()
		var entry cacheEntry
		if found {
			entry = cacheEntry{Limits: limits, ExpiresAt: now.Add(r.ttlFor(limits.Source)), StaleUntil: now.Add(time.Duration(r.config.StaleTTLHours) * time.Hour)}
		} else if hasStale && now.Before(stale.StaleUntil) && !stale.Negative {
			limits = stale.Limits
			limits.Cached = true
			found = true
			entry = stale
		} else {
			limits = r.fallbackLimits(now)
			entry = cacheEntry{Limits: limits, Negative: true, ExpiresAt: now.Add(time.Duration(r.config.NegativeTTLMinutes) * time.Minute), StaleUntil: now.Add(time.Duration(r.config.NegativeTTLMinutes) * time.Minute)}
		}
		r.mu.Lock()
		r.entries[key] = entry
		r.pruneLocked()
		_ = r.saveCacheLocked()
		delete(r.inflight, key)
		close(wait)
		r.mu.Unlock()
		return snapshot.WithLimits(limits), nil
	}
}

func (r *LimitResolver) ResolveMany(ctx context.Context, snapshots []Snapshot, apiKey func(config.ProviderConfig) string, workers int) []Snapshot {
	if workers <= 0 || workers > 4 {
		workers = 4
	}
	result := append([]Snapshot(nil), snapshots...)
	jobs := make(chan int)
	var group sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		group.Add(1)
		go func() {
			defer group.Done()
			for index := range jobs {
				resolved, err := r.Resolve(ctx, result[index], apiKey(result[index].Config))
				if err == nil {
					result[index] = resolved
				} else {
					result[index] = result[index].WithLimits(r.fallbackLimits(r.now().UTC()))
				}
			}
		}()
	}
	for index := range result {
		select {
		case <-ctx.Done():
			close(jobs)
			group.Wait()
			return result
		case jobs <- index:
		}
	}
	close(jobs)
	group.Wait()
	return result
}

func (r *LimitResolver) LearnOverflow(snapshot Snapshot, confirmedLimit int) Snapshot {
	current := snapshot.EffectiveContextWindow
	if current <= 0 {
		current = snapshot.Limits.ContextWindow
	}
	limit := confirmedLimit
	if limit > 0 && current > 0 && limit >= current {
		limit = 0
	}
	if limit <= 0 {
		limit = nextTier(r.config.ProbeTiers, current)
	}
	if limit <= 0 {
		limit = r.config.ProbeTiers[len(r.config.ProbeTiers)-1]
	}
	now := r.now().UTC()
	limits := ModelLimits{ContextWindow: limit, Source: LimitSourceOverflow, ObservedAt: now, Degradations: snapshot.Limits.Degradations + 1}
	entry := cacheEntry{Limits: limits, ExpiresAt: now.Add(time.Duration(r.config.LearnedTTLHours) * time.Hour), StaleUntil: now.Add(time.Duration(r.config.LearnedTTLHours) * time.Hour)}
	r.mu.Lock()
	r.loadCacheLocked()
	r.entries[metadataKey(snapshot.Config)] = entry
	r.pruneLocked()
	_ = r.saveCacheLocked()
	r.mu.Unlock()
	return snapshot.WithLimits(limits)
}

func (r *LimitResolver) resolveRemote(ctx context.Context, snapshot Snapshot, apiKey string) (ModelLimits, bool) {
	timeout := time.Duration(r.config.RequestTimeoutSeconds) * time.Second
	requestCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if limits, ok := r.resolveEndpoint(requestCtx, snapshot.Config, apiKey); ok {
		return limits, true
	}
	if requestCtx.Err() != nil {
		return ModelLimits{}, false
	}
	return r.resolveCatalog(requestCtx, snapshot.Config)
}

func (r *LimitResolver) resolveEndpoint(ctx context.Context, provider config.ProviderConfig, apiKey string) (ModelLimits, bool) {
	if limits, ok := r.resolveModelList(ctx, provider, apiKey); ok {
		return limits, true
	}
	parsed, err := url.Parse(provider.BaseURL)
	if err != nil || !isLocalHost(parsed.Hostname()) {
		return ModelLimits{}, false
	}
	root := endpointRoot(parsed)
	if limits, ok := r.resolveOllamaPS(ctx, root, provider, apiKey); ok {
		return limits, true
	}
	if limits, ok := r.resolveOllamaShow(ctx, root, provider, apiKey); ok {
		return limits, true
	}
	return r.resolveLlamaCPP(ctx, root, provider, apiKey)
}

func (r *LimitResolver) resolveModelList(ctx context.Context, provider config.ProviderConfig, apiKey string) (ModelLimits, bool) {
	endpoint := strings.TrimRight(provider.BaseURL, "/") + "/models"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return ModelLimits{}, false
	}
	applyProviderHeaders(req, provider, apiKey)
	raw, ok := r.readJSON(req)
	if !ok {
		return ModelLimits{}, false
	}
	var payload struct {
		Data []struct {
			ID                  string `json:"id"`
			ContextLength       int    `json:"context_length"`
			ContextWindow       int    `json:"context_window"`
			MaxContextLength    int    `json:"max_context_length"`
			MaxCompletionTokens int    `json:"max_completion_tokens"`
			TopProvider         struct {
				ContextLength       int `json:"context_length"`
				MaxCompletionTokens int `json:"max_completion_tokens"`
			} `json:"top_provider"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &payload) != nil {
		return ModelLimits{}, false
	}
	for _, model := range payload.Data {
		if model.ID != provider.Model {
			continue
		}
		window := firstPositive(model.TopProvider.ContextLength, model.ContextLength, model.ContextWindow, model.MaxContextLength)
		if window <= 0 {
			return ModelLimits{}, false
		}
		source := LimitSourceOpenAIExtension
		if parsed, err := url.Parse(provider.BaseURL); err == nil && strings.Contains(strings.ToLower(parsed.Hostname()), "openrouter.ai") {
			source = LimitSourceOpenRouter
		}
		return ModelLimits{ContextWindow: window, MaxOutputTokens: firstPositive(model.TopProvider.MaxCompletionTokens, model.MaxCompletionTokens), Source: source, ObservedAt: r.now().UTC()}, true
	}
	return ModelLimits{}, false
}

func (r *LimitResolver) resolveOllamaPS(ctx context.Context, root string, provider config.ProviderConfig, apiKey string) (ModelLimits, bool) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, root+"/api/ps", nil)
	applyProviderHeaders(req, provider, apiKey)
	raw, ok := r.readJSON(req)
	if !ok {
		return ModelLimits{}, false
	}
	var payload struct {
		Models []struct {
			Name          string `json:"name"`
			Model         string `json:"model"`
			ContextLength int    `json:"context_length"`
		} `json:"models"`
	}
	if json.Unmarshal(raw, &payload) != nil {
		return ModelLimits{}, false
	}
	for _, model := range payload.Models {
		if model.ContextLength > 0 && (modelMatches(provider.Model, model.Name) || modelMatches(provider.Model, model.Model)) {
			return ModelLimits{ContextWindow: model.ContextLength, Source: LimitSourceOllamaPS, ObservedAt: r.now().UTC()}, true
		}
	}
	return ModelLimits{}, false
}

func (r *LimitResolver) resolveOllamaShow(ctx context.Context, root string, provider config.ProviderConfig, apiKey string) (ModelLimits, bool) {
	payload, _ := json.Marshal(map[string]string{"model": provider.Model})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, root+"/api/show", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	applyProviderHeaders(req, provider, apiKey)
	raw, ok := r.readJSON(req)
	if !ok {
		return ModelLimits{}, false
	}
	var decoded struct {
		Parameters any            `json:"parameters"`
		ModelInfo  map[string]any `json:"model_info"`
	}
	if json.Unmarshal(raw, &decoded) != nil {
		return ModelLimits{}, false
	}
	window := parameterContext(decoded.Parameters)
	for key, value := range decoded.ModelInfo {
		if strings.HasSuffix(key, ".context_length") || key == "context_length" {
			window = minPositive(window, numberAsInt(value))
		}
	}
	if window <= 0 {
		return ModelLimits{}, false
	}
	return ModelLimits{ContextWindow: window, Source: LimitSourceOllamaShow, ObservedAt: r.now().UTC()}, true
}

func (r *LimitResolver) resolveLlamaCPP(ctx context.Context, root string, provider config.ProviderConfig, apiKey string) (ModelLimits, bool) {
	endpoint := root + "/props?model=" + url.QueryEscape(provider.Model)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	applyProviderHeaders(req, provider, apiKey)
	raw, ok := r.readJSON(req)
	if !ok {
		return ModelLimits{}, false
	}
	var payload struct {
		DefaultGenerationSettings struct {
			Context int `json:"n_ctx"`
		} `json:"default_generation_settings"`
	}
	if json.Unmarshal(raw, &payload) != nil || payload.DefaultGenerationSettings.Context <= 0 {
		return ModelLimits{}, false
	}
	return ModelLimits{ContextWindow: payload.DefaultGenerationSettings.Context, Source: LimitSourceLlamaCPP, ObservedAt: r.now().UTC()}, true
}

func (r *LimitResolver) resolveCatalog(ctx context.Context, provider config.ProviderConfig) (ModelLimits, bool) {
	catalog, ok := r.catalogData(ctx)
	if !ok {
		return ModelLimits{}, false
	}
	providerID, modelID := catalogIdentity(catalog, provider)
	if providerID == "" {
		return ModelLimits{}, false
	}
	entry, ok := catalog[providerID].Models[modelID]
	if !ok || entry.Limit.Context <= 0 {
		return ModelLimits{}, false
	}
	return ModelLimits{ContextWindow: entry.Limit.Context, MaxOutputTokens: entry.Limit.Output, Source: LimitSourceModelsDev, ObservedAt: r.now().UTC()}, true
}

func (r *LimitResolver) catalogData(ctx context.Context) (map[string]catalogProvider, bool) {
	r.catalogMu.Lock()
	defer r.catalogMu.Unlock()
	if r.catalog != nil && r.now().Before(r.catalogAt.Add(time.Duration(r.config.CatalogTTLHours)*time.Hour)) {
		return r.catalog, true
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.config.CatalogURL, nil)
	if err != nil {
		return nil, false
	}
	raw, ok := r.readJSON(req)
	if !ok {
		return nil, false
	}
	var catalog map[string]catalogProvider
	if json.Unmarshal(raw, &catalog) != nil {
		return nil, false
	}
	r.catalog, r.catalogAt = catalog, r.now().UTC()
	return catalog, true
}

func (r *LimitResolver) readJSON(req *http.Request) ([]byte, bool) {
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, false
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, false
	}
	reader := io.LimitReader(resp.Body, int64(r.config.MaxResponseBytes)+1)
	raw, err := io.ReadAll(reader)
	if err != nil || len(raw) > r.config.MaxResponseBytes {
		return nil, false
	}
	return raw, true
}

func (r *LimitResolver) fallbackLimits(now time.Time) ModelLimits {
	return ModelLimits{ContextWindow: r.config.ProbeTiers[0], Source: LimitSourceFallback, ObservedAt: now, Assumed: true}
}

func (r *LimitResolver) ttlFor(source LimitSource) time.Duration {
	if source == LimitSourceModelsDev {
		return time.Duration(r.config.CatalogTTLHours) * time.Hour
	}
	if source == LimitSourceOverflow {
		return time.Duration(r.config.LearnedTTLHours) * time.Hour
	}
	return time.Duration(r.config.EndpointTTLHours) * time.Hour
}

func (r *LimitResolver) finishInflight(key string, wait chan struct{}) {
	r.mu.Lock()
	if current, ok := r.inflight[key]; ok && current == wait {
		delete(r.inflight, key)
		close(wait)
	}
	r.mu.Unlock()
}

func (r *LimitResolver) loadCacheLocked() {
	if r.loaded {
		return
	}
	r.loaded = true
	raw, err := os.ReadFile(r.cachePath)
	if err != nil {
		return
	}
	var file metadataCacheFile
	if json.Unmarshal(raw, &file) == nil && file.Version == 1 && file.Entries != nil {
		r.entries = file.Entries
	}
}

func (r *LimitResolver) saveCacheLocked() error {
	file := metadataCacheFile{Version: 1, Entries: r.entries}
	raw, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(r.cachePath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".model-metadata-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, r.cachePath); err != nil {
		if removeErr := os.Remove(r.cachePath); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			return err
		}
		return os.Rename(name, r.cachePath)
	}
	return nil
}

func (r *LimitResolver) pruneLocked() {
	if len(r.entries) <= r.config.MaxCacheEntries {
		return
	}
	type item struct {
		key string
		at  time.Time
	}
	items := make([]item, 0, len(r.entries))
	for key, entry := range r.entries {
		items = append(items, item{key: key, at: entry.Limits.ObservedAt})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].at.Before(items[j].at) })
	for _, item := range items[:len(items)-r.config.MaxCacheEntries] {
		delete(r.entries, item.key)
	}
}

func metadataKey(provider config.ProviderConfig) string {
	base := strings.TrimRight(strings.TrimSpace(provider.BaseURL), "/")
	if parsed, err := url.Parse(base); err == nil {
		parsed.Scheme = strings.ToLower(parsed.Scheme)
		parsed.Host = strings.ToLower(parsed.Host)
		base = strings.TrimRight(parsed.String(), "/")
	}
	sum := sha256.Sum256([]byte(base + "\x00" + provider.Adapter + "\x00" + provider.Model + "\x00" + provider.CatalogProvider))
	return hex.EncodeToString(sum[:])
}

func catalogIdentity(catalog map[string]catalogProvider, provider config.ProviderConfig) (string, string) {
	modelID := provider.Model
	if provider.CatalogProvider != "" {
		return findCatalogProvider(catalog, provider.CatalogProvider), trimProviderPrefix(modelID, provider.CatalogProvider)
	}
	if prefix, rest, ok := strings.Cut(modelID, "/"); ok {
		if id := findCatalogProvider(catalog, prefix); id != "" {
			return id, rest
		}
	}
	if parsed, err := url.Parse(provider.BaseURL); err == nil {
		host := strings.ToLower(parsed.Hostname())
		for domain, id := range map[string]string{"api.openai.com": "openai", "api.anthropic.com": "anthropic", "generativelanguage.googleapis.com": "google"} {
			if host == domain {
				if found := findCatalogProvider(catalog, id); found != "" {
					return found, modelID
				}
			}
		}
	}
	found := ""
	for id, candidate := range catalog {
		if _, ok := candidate.Models[modelID]; ok {
			if found != "" {
				return "", ""
			}
			found = id
		}
	}
	return found, modelID
}

func findCatalogProvider(catalog map[string]catalogProvider, requested string) string {
	if _, ok := catalog[requested]; ok {
		return requested
	}
	for id, provider := range catalog {
		if provider.ID == requested {
			return id
		}
	}
	return ""
}

func trimProviderPrefix(model, provider string) string {
	prefix := provider + "/"
	if strings.HasPrefix(model, prefix) {
		return strings.TrimPrefix(model, prefix)
	}
	return model
}

func endpointRoot(parsed *url.URL) string {
	copy := *parsed
	copy.RawQuery, copy.Fragment = "", ""
	copy.Path = strings.TrimSuffix(strings.TrimRight(copy.Path, "/"), "/v1")
	return strings.TrimRight(copy.String(), "/")
}

func applyProviderHeaders(req *http.Request, provider config.ProviderConfig, apiKey string) {
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	for key, value := range provider.Headers {
		req.Header.Set(key, value)
	}
}

func isLocalHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func modelMatches(want, got string) bool {
	if want == got {
		return true
	}
	return strings.TrimSuffix(want, ":latest") == strings.TrimSuffix(got, ":latest")
}

func parameterContext(value any) int {
	switch typed := value.(type) {
	case string:
		fields := strings.Fields(typed)
		for index := 0; index+1 < len(fields); index++ {
			if fields[index] == "num_ctx" {
				parsed, _ := strconv.Atoi(fields[index+1])
				return parsed
			}
		}
	case map[string]any:
		return numberAsInt(typed["num_ctx"])
	}
	return 0
}

func numberAsInt(value any) int {
	switch typed := value.(type) {
	case float64:
		return int(typed)
	case json.Number:
		parsed, _ := strconv.Atoi(typed.String())
		return parsed
	case int:
		return typed
	case string:
		parsed, _ := strconv.Atoi(typed)
		return parsed
	default:
		return 0
	}
}

func firstPositive(values ...int) int {
	for _, value := range values {
		if value > 0 {
			return value
		}
	}
	return 0
}
func minPositive(left, right int) int {
	if left <= 0 {
		return right
	}
	if right <= 0 || left < right {
		return left
	}
	return right
}

func nextTier(tiers []int, current int) int {
	if len(tiers) == 0 {
		return 0
	}
	if current <= 0 {
		return tiers[0]
	}
	for _, tier := range tiers {
		if tier < current {
			return tier
		}
	}
	return 0
}
