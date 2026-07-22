package webtool

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strings"
	"sync"

	"Eylu/internal/config"
	"Eylu/internal/policy"
	"Eylu/internal/protocol"
	"Eylu/internal/tool"
)

type DelegateFunc func(context.Context, ResolvedTool, json.RawMessage) protocol.ToolResult

type UsageBudget struct {
	mu   sync.Mutex
	uses map[protocol.ToolKind]int
}

func NewUsageBudget() *UsageBudget { return &UsageBudget{uses: make(map[protocol.ToolKind]int)} }

func (b *UsageBudget) Record(kind protocol.ToolKind, maximum int) bool {
	if b == nil {
		return true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if maximum <= 0 {
		maximum = 5
	}
	if b.uses[kind] >= maximum {
		return false
	}
	b.uses[kind]++
	return true
}

func (b *UsageBudget) acquire(kind protocol.ToolKind, maximum int) bool {
	return b.Record(kind, maximum)
}

type LocalTool struct {
	resolved     ResolvedTool
	target       tool.Tool
	delegate     DelegateFunc
	permission   string
	budget       *UsageBudget
	targetSchema map[string]json.RawMessage
}

func NewLocalTool(resolved ResolvedTool, target tool.Tool, delegate DelegateFunc, permission string, budget *UsageBudget) *LocalTool {
	item := &LocalTool{resolved: resolved, target: target, delegate: delegate, permission: permission, budget: budget}
	if target != nil {
		var schema struct {
			Properties map[string]json.RawMessage `json:"properties"`
		}
		_ = json.Unmarshal(target.Definition().InputSchema, &schema)
		item.targetSchema = schema.Properties
	}
	return item
}

func (t *LocalTool) Definition() protocol.ToolDefinition {
	definition := t.resolved.Definition
	readOnly, openWorld := true, true
	definition.Annotations = &protocol.ToolAnnotations{Title: definition.Name, ReadOnlyHint: readOnly, OpenWorldHint: &openWorld}
	return definition
}

func (t *LocalTool) Risk() policy.Risk  { return policy.RiskNetwork }
func (t *LocalTool) ParallelSafe() bool { return true }

func (t *LocalTool) OverridePolicy(json.RawMessage) (policy.Outcome, bool) {
	permission := strings.ToLower(strings.TrimSpace(t.permission))
	if permission == "" {
		permission = config.WebPermissionAsk
	}
	outcome := policy.Outcome{Risk: policy.RiskNetwork, Classification: policy.CommandNotApplicable, Reason: "web access policy"}
	switch permission {
	case config.WebPermissionAllow:
		outcome.Decision = policy.DecisionAllow
	case config.WebPermissionDeny:
		outcome.Decision = policy.DecisionDeny
	default:
		outcome.Decision = policy.DecisionConfirm
		outcome.Confirmations = 1
	}
	return outcome, true
}

func (t *LocalTool) Execute(ctx context.Context, raw json.RawMessage) protocol.ToolResult {
	if !t.budget.acquire(t.resolved.Definition.Kind, t.resolved.Definition.MaxUses) {
		return protocol.ToolResult{Content: fmt.Sprintf("%s max_uses exceeded", t.resolved.Definition.Kind), IsError: true, Metadata: t.metadata()}
	}
	var input map[string]any
	if err := json.Unmarshal(raw, &input); err != nil {
		return protocol.ToolResult{Content: "invalid web tool input: " + err.Error(), IsError: true, Metadata: t.metadata()}
	}
	field := "query"
	if t.resolved.Definition.Kind == protocol.ToolWebFetch {
		field = "url"
	}
	value, _ := input[field].(string)
	value = strings.TrimSpace(value)
	if value == "" {
		return protocol.ToolResult{Content: fmt.Sprintf("web tool input requires %q", field), IsError: true, Metadata: t.metadata()}
	}
	if field == "url" {
		if err := ValidateURL(ctx, value, t.resolved.Definition.AllowedDomains, t.resolved.Definition.BlockedDomains); err != nil {
			return protocol.ToolResult{Content: "web fetch denied: " + err.Error(), IsError: true, Metadata: t.metadata()}
		}
	}
	if t.resolved.Execution == protocol.ExecutionDelegated {
		if t.delegate == nil {
			return protocol.ToolResult{Content: "delegated web backend is unavailable", IsError: true, Metadata: t.metadata()}
		}
		result := t.delegate(ctx, t.resolved, raw)
		result.Metadata = mergeMetadata(result.Metadata, t.metadata())
		return result
	}
	if t.target == nil {
		return protocol.ToolResult{Content: "MCP web backend is unavailable", IsError: true, Metadata: t.metadata()}
	}
	targetInput := map[string]any{field: value}
	if _, ok := t.targetSchema["allowed_domains"]; ok && len(t.resolved.Definition.AllowedDomains) > 0 {
		targetInput["allowed_domains"] = t.resolved.Definition.AllowedDomains
	}
	if _, ok := t.targetSchema["blocked_domains"]; ok && len(t.resolved.Definition.BlockedDomains) > 0 {
		targetInput["blocked_domains"] = t.resolved.Definition.BlockedDomains
	}
	encoded, _ := json.Marshal(targetInput)
	result := t.target.Execute(ctx, encoded)
	result.Metadata = mergeMetadata(result.Metadata, t.metadata())
	return result
}

func (t *LocalTool) metadata() map[string]any {
	return map[string]any{
		"web_backend": string(t.resolved.Execution), "web_kind": string(t.resolved.Definition.Kind),
		"web_target": t.resolved.Target, "untrusted_web_content": true,
	}
}

func mergeMetadata(existing, added map[string]any) map[string]any {
	if existing == nil {
		existing = make(map[string]any, len(added))
	}
	for key, value := range added {
		existing[key] = value
	}
	return existing
}

func ValidateURL(ctx context.Context, raw string, allowed, blocked []string) error {
	return validateURL(ctx, raw, allowed, blocked, net.DefaultResolver.LookupIP)
}

func validateURL(ctx context.Context, raw string, allowed, blocked []string, lookup func(context.Context, string, string) ([]net.IP, error)) error {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Hostname() == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return fmt.Errorf("URL must be absolute HTTP(S)")
	}
	if parsed.User != nil {
		return fmt.Errorf("URL credentials are forbidden")
	}
	host := normalizeDomain(parsed.Hostname())
	if domainMatchesAny(host, blocked) {
		return fmt.Errorf("domain %q is blocked", host)
	}
	if len(allowed) > 0 && !domainMatchesAny(host, allowed) {
		return fmt.Errorf("domain %q is outside allowed_domains", host)
	}
	if address := net.ParseIP(host); address != nil {
		if isSpecialAddress(address) {
			return fmt.Errorf("special-purpose IP addresses are forbidden")
		}
		return nil
	}
	addresses, err := lookup(ctx, "ip", host)
	if err != nil {
		return fmt.Errorf("resolve host: %w", err)
	}
	if len(addresses) == 0 {
		return fmt.Errorf("host resolved without addresses")
	}
	for _, address := range addresses {
		if isSpecialAddress(address) {
			return fmt.Errorf("host resolves to a special-purpose IP address")
		}
	}
	return nil
}

func normalizeDomain(value string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(value)), ".")
}

func domainMatchesAny(host string, rules []string) bool {
	for _, rule := range rules {
		rule = strings.TrimPrefix(normalizeDomain(rule), "*.")
		if rule != "" && (host == rule || strings.HasSuffix(host, "."+rule)) {
			return true
		}
	}
	return false
}

var specialPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("169.254.0.0/16"), netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.0.0.0/24"), netip.MustParsePrefix("192.0.2.0/24"), netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("198.18.0.0/15"), netip.MustParsePrefix("198.51.100.0/24"), netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("224.0.0.0/4"), netip.MustParsePrefix("240.0.0.0/4"), netip.MustParsePrefix("::/128"),
	netip.MustParsePrefix("::1/128"), netip.MustParsePrefix("fc00::/7"), netip.MustParsePrefix("fe80::/10"), netip.MustParsePrefix("ff00::/8"),
	netip.MustParsePrefix("2001:db8::/32"),
}

func isSpecialAddress(ip net.IP) bool {
	address, ok := netip.AddrFromSlice(ip)
	if !ok {
		return true
	}
	address = address.Unmap()
	if !address.IsGlobalUnicast() || address.IsPrivate() || address.IsLoopback() || address.IsLinkLocalUnicast() || address.IsMulticast() || address.IsUnspecified() {
		return true
	}
	for _, prefix := range specialPrefixes {
		if prefix.Contains(address) {
			return true
		}
	}
	return false
}

var _ tool.Tool = (*LocalTool)(nil)
var _ tool.PolicyOverride = (*LocalTool)(nil)
