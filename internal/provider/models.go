package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	"Eylu/internal/protocol"
)

type ModelLister struct {
	client *http.Client
}

func NewModelLister(client *http.Client) *ModelLister {
	if client == nil {
		client = http.DefaultClient
	}
	return &ModelLister{client: client}
}

func (l *ModelLister) List(ctx context.Context, baseURL, apiKey string, headers map[string]string) ([]string, error) {
	endpoint := strings.TrimRight(baseURL, "/") + "/models"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	resp, err := l.client.Do(req)
	if err != nil {
		return nil, &protocol.Error{Code: protocol.ErrNetwork, Message: "list provider models", Retryable: true, Cause: err}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &protocol.Error{Code: protocol.ErrProvider, Message: fmt.Sprintf("model list HTTP %d", resp.StatusCode), StatusCode: resp.StatusCode}
	}
	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, &protocol.Error{Code: protocol.ErrProtocol, Message: "decode model list", Cause: err}
	}
	seen := make(map[string]struct{})
	models := make([]string, 0, len(payload.Data))
	for _, item := range payload.Data {
		if item.ID == "" {
			continue
		}
		if _, ok := seen[item.ID]; ok {
			continue
		}
		seen[item.ID] = struct{}{}
		models = append(models, item.ID)
	}
	sort.Strings(models)
	return models, nil
}
