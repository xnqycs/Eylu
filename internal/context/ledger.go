package context

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"

	"Eylu/internal/protocol"
)

type Category string

const (
	CategorySystemPrompt      Category = "system_prompt"
	CategorySkillCatalog      Category = "skill_catalog"
	CategorySkillBody         Category = "skill_body"
	CategorySkillResource     Category = "skill_resource"
	CategoryMCPInstructions   Category = "mcp_instructions"
	CategoryMCPToolSchema     Category = "mcp_tool_schema"
	CategoryMCPResource       Category = "mcp_resource"
	CategoryMCPToolResult     Category = "mcp_tool_result"
	CategoryBuiltinToolSchema Category = "builtin_tool_schema"
	CategoryUserMessage       Category = "user_message"
	CategoryAgentMessage      Category = "agent_message"
	CategoryBuiltinToolResult Category = "builtin_tool_result"
	CategoryProjectContext    Category = "project_context"
	CategorySummary           Category = "summary"
	CategoryDriverState       Category = "driver_state"
	CategoryOutputReserve     Category = "output_reserve"
)

var categoryOrder = []Category{
	CategorySystemPrompt,
	CategorySkillCatalog,
	CategorySkillBody,
	CategorySkillResource,
	CategoryMCPInstructions,
	CategoryMCPToolSchema,
	CategoryMCPResource,
	CategoryMCPToolResult,
	CategoryBuiltinToolSchema,
	CategoryUserMessage,
	CategoryAgentMessage,
	CategoryBuiltinToolResult,
	CategoryProjectContext,
	CategorySummary,
	CategoryDriverState,
	CategoryOutputReserve,
}

var categoryLabels = map[Category]string{
	CategorySystemPrompt:      "System prompt",
	CategorySkillCatalog:      "Skill catalog",
	CategorySkillBody:         "Skill body",
	CategorySkillResource:     "Skill resources",
	CategoryMCPInstructions:   "MCP instructions",
	CategoryMCPToolSchema:     "MCP tool schemas",
	CategoryMCPResource:       "MCP resources",
	CategoryMCPToolResult:     "MCP tool results",
	CategoryBuiltinToolSchema: "Tool schemas",
	CategoryUserMessage:       "User messages",
	CategoryAgentMessage:      "Agent messages",
	CategoryBuiltinToolResult: "Tool results",
	CategoryProjectContext:    "Project context",
	CategorySummary:           "Summary",
	CategoryDriverState:       "Driver state",
	CategoryOutputReserve:     "Output reserve",
}

type TokenEstimator interface {
	Estimate(string) int
}

type ApproxEstimator struct {
	BytesPerToken int
}

func (e ApproxEstimator) Estimate(text string) int {
	ratio := e.BytesPerToken
	if ratio <= 0 {
		ratio = 4
	}
	if text == "" {
		return 0
	}
	return (len([]byte(text)) + ratio - 1) / ratio
}

type Block struct {
	ID        string         `json:"id"`
	Category  Category       `json:"category"`
	Source    string         `json:"source"`
	Bytes     int            `json:"bytes"`
	Tokens    int            `json:"tokens"`
	Exact     bool           `json:"exact"`
	Protected bool           `json:"protected,omitempty"`
	Metadata  map[string]any `json:"metadata,omitempty"`
}

type SourceUsage struct {
	Source      string  `json:"source"`
	Blocks      int     `json:"blocks"`
	Bytes       int     `json:"bytes"`
	Tokens      int     `json:"tokens"`
	Percent     float64 `json:"percent"`
	Exact       bool    `json:"exact"`
	Measurement string  `json:"measurement"`
}

type CategoryUsage struct {
	Category    Category      `json:"category"`
	Label       string        `json:"label"`
	Blocks      int           `json:"blocks"`
	Bytes       int           `json:"bytes"`
	Tokens      int           `json:"tokens"`
	Exact       bool          `json:"exact"`
	Percent     float64       `json:"percent"`
	Measurement string        `json:"measurement"`
	Sources     []SourceUsage `json:"sources,omitempty"`
}

type Report struct {
	Provider         string            `json:"provider"`
	Model            string            `json:"model"`
	ContextWindow    int               `json:"context_window,omitempty"`
	LimitSource      string            `json:"limit_source"`
	InputTokens      int               `json:"input_tokens"`
	OutputReserve    int               `json:"output_reserve"`
	TotalTokens      int               `json:"total_tokens"`
	Percent          float64           `json:"percent,omitempty"`
	LimitKnown       bool              `json:"limit_known"`
	Categories       []CategoryUsage   `json:"categories"`
	LastUsage        protocol.Usage    `json:"last_provider_usage"`
	MeasurementKind  string            `json:"measurement_kind"`
	CompressionCount int               `json:"compression_count"`
	LastCompression  *CompressionEvent `json:"last_compression,omitempty"`
}

type Ledger struct {
	mu               sync.RWMutex
	estimator        TokenEstimator
	blocks           []Block
	lastUsage        protocol.Usage
	compressionCount int
	lastCompression  *CompressionEvent
}

func New(estimator TokenEstimator) *Ledger {
	if estimator == nil {
		estimator = ApproxEstimator{BytesPerToken: 4}
	}
	return &Ledger{estimator: estimator}
}

func (l *Ledger) Reset() {
	l.mu.Lock()
	l.blocks = nil
	l.lastUsage = protocol.Usage{}
	l.compressionCount = 0
	l.lastCompression = nil
	l.mu.Unlock()
}

func (l *Ledger) SetEstimator(estimator TokenEstimator) {
	if estimator == nil {
		estimator = ApproxEstimator{BytesPerToken: 4}
	}
	l.mu.Lock()
	l.estimator = estimator
	l.mu.Unlock()
}

func (l *Ledger) ReplaceBlocks(blocks []Block) {
	l.mu.Lock()
	l.blocks = append(l.blocks[:0], blocks...)
	l.mu.Unlock()
}

func (l *Ledger) AddText(id string, category Category, source, text string, protected bool) Block {
	block := Block{ID: id, Category: category, Source: source, Bytes: len([]byte(text)), Tokens: l.estimator.Estimate(text), Exact: false, Protected: protected}
	l.Add(block)
	return block
}

func (l *Ledger) Add(block Block) {
	l.mu.Lock()
	l.blocks = append(l.blocks, block)
	l.mu.Unlock()
}

func (l *Ledger) SetLastUsage(usage protocol.Usage) {
	l.mu.Lock()
	l.lastUsage = usage
	l.mu.Unlock()
}

func (l *Ledger) RecordCompression(event CompressionEvent) {
	l.mu.Lock()
	l.compressionCount++
	copy := event
	l.lastCompression = &copy
	l.mu.Unlock()
}

func (l *Ledger) Blocks() []Block {
	l.mu.RLock()
	defer l.mu.RUnlock()
	result := make([]Block, len(l.blocks))
	copy(result, l.blocks)
	return result
}

func (l *Ledger) Report(providerName, model string, contextWindow int) Report {
	l.mu.RLock()
	defer l.mu.RUnlock()
	usage := make(map[Category]*CategoryUsage, len(categoryOrder))
	sources := make(map[Category]map[string]*SourceUsage, len(categoryOrder))
	for _, category := range categoryOrder {
		usage[category] = &CategoryUsage{Category: category, Label: categoryLabels[category], Exact: true}
	}
	for _, block := range l.blocks {
		item, ok := usage[block.Category]
		if !ok {
			item = &CategoryUsage{Category: block.Category, Label: string(block.Category), Exact: true}
			usage[block.Category] = item
		}
		item.Blocks++
		item.Bytes += block.Bytes
		item.Tokens += block.Tokens
		item.Exact = item.Exact && block.Exact
		if sources[block.Category] == nil {
			sources[block.Category] = make(map[string]*SourceUsage)
		}
		source := sources[block.Category][block.Source]
		if source == nil {
			source = &SourceUsage{Source: block.Source, Exact: true}
			sources[block.Category][block.Source] = source
		}
		source.Blocks++
		source.Bytes += block.Bytes
		source.Tokens += block.Tokens
		source.Exact = source.Exact && block.Exact
	}
	report := Report{Provider: providerName, Model: model, ContextWindow: contextWindow, LastUsage: l.lastUsage, MeasurementKind: "estimated", CompressionCount: l.compressionCount}
	if l.lastCompression != nil {
		copy := *l.lastCompression
		report.LastCompression = &copy
	}
	for _, category := range categoryOrder {
		item := *usage[category]
		item.Measurement = measurement(item.Exact)
		for _, source := range sortedSources(sources[category]) {
			copy := *source
			copy.Measurement = measurement(copy.Exact)
			item.Sources = append(item.Sources, copy)
		}
		report.Categories = append(report.Categories, item)
		if category == CategoryOutputReserve {
			report.OutputReserve += item.Tokens
		} else {
			report.InputTokens += item.Tokens
		}
	}
	unknown := make([]Category, 0)
	for category := range usage {
		found := false
		for _, known := range categoryOrder {
			if category == known {
				found = true
				break
			}
		}
		if !found {
			unknown = append(unknown, category)
		}
	}
	sort.Slice(unknown, func(i, j int) bool { return unknown[i] < unknown[j] })
	for _, category := range unknown {
		item := *usage[category]
		item.Measurement = measurement(item.Exact)
		for _, source := range sortedSources(sources[category]) {
			copy := *source
			copy.Measurement = measurement(copy.Exact)
			item.Sources = append(item.Sources, copy)
		}
		report.Categories = append(report.Categories, item)
		report.InputTokens += item.Tokens
	}
	report.TotalTokens = report.InputTokens + report.OutputReserve
	for index := range report.Categories {
		item := &report.Categories[index]
		if report.InputTokens > 0 && item.Category != CategoryOutputReserve {
			item.Percent = float64(item.Tokens) / float64(report.InputTokens) * 100
		}
		for sourceIndex := range item.Sources {
			if item.Tokens > 0 {
				item.Sources[sourceIndex].Percent = float64(item.Sources[sourceIndex].Tokens) / float64(item.Tokens) * 100
			}
		}
	}
	if contextWindow > 0 {
		report.LimitSource = "provider_config"
		report.LimitKnown = true
		report.Percent = float64(report.TotalTokens) / float64(contextWindow) * 100
	} else {
		report.LimitSource = "unknown"
	}
	return report
}

func RenderText(w io.Writer, report Report) error {
	limit := "unknown"
	percent := "unknown"
	if report.ContextWindow > 0 {
		limit = fmt.Sprintf("%d", report.ContextWindow)
		percent = fmt.Sprintf("%.1f%%", report.Percent)
	}
	if _, err := fmt.Fprintf(w, "Context  %d input + %d reserved / %s  (%s)\n", report.InputTokens, report.OutputReserve, limit, percent); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "Provider %s · model %s · limit %s\n\n", report.Provider, report.Model, report.LimitSource); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(w, "Category                 Blocks    Tokens   Input"); err != nil {
		return err
	}
	for _, item := range report.Categories {
		share := 0.0
		if report.InputTokens > 0 && item.Category != CategoryOutputReserve {
			share = float64(item.Tokens) / float64(report.InputTokens) * 100
		}
		mark := "estimated"
		if item.Exact {
			mark = "exact"
		}
		if _, err := fmt.Fprintf(w, "%-24s %6d %9d %6.1f%%  %s\n", truncate(item.Label, 24), item.Blocks, item.Tokens, share, mark); err != nil {
			return err
		}
		if expandableCategory(item.Category) {
			for _, source := range item.Sources {
				if _, err := fmt.Fprintf(w, "  - %-20s %6d %9d %6.1f%%  %s\n", truncate(source.Source, 20), source.Blocks, source.Tokens, source.Percent, source.Measurement); err != nil {
					return err
				}
			}
		}
	}
	if report.LastUsage.InputTokens > 0 || report.LastUsage.OutputTokens > 0 {
		_, err := fmt.Fprintf(w, "\nLast provider usage: %d input, %d output\n", report.LastUsage.InputTokens, report.LastUsage.OutputTokens)
		return err
	}
	return nil
}

func sortedSources(items map[string]*SourceUsage) []*SourceUsage {
	result := make([]*SourceUsage, 0, len(items))
	for _, item := range items {
		result = append(result, item)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Source < result[j].Source })
	return result
}

func measurement(exact bool) string {
	if exact {
		return "exact"
	}
	return "estimated"
}

func expandableCategory(category Category) bool {
	switch category {
	case CategorySkillCatalog, CategorySkillBody, CategorySkillResource, CategoryMCPInstructions, CategoryMCPToolSchema, CategoryMCPResource, CategoryMCPToolResult:
		return true
	default:
		return false
	}
}

func truncate(value string, width int) string {
	if len(value) <= width {
		return value
	}
	return strings.TrimSpace(value[:width])
}
