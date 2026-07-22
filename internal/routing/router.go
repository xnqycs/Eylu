package routing

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"Eylu/internal/driver"
	"Eylu/internal/provider"
)

const (
	TaskGeneral       = "general"
	TaskCoding        = "coding"
	TaskReview        = "review"
	TaskDebugging     = "debugging"
	TaskTesting       = "testing"
	TaskDocumentation = "documentation"
)

type Request struct {
	Task            string              `json:"task"`
	RequiredContext int                 `json:"required_context"`
	EstimatedInput  int                 `json:"estimated_input_tokens"`
	EstimatedOutput int                 `json:"estimated_output_tokens"`
	Capabilities    driver.Capabilities `json:"required_capabilities"`
}

type Candidate struct {
	Provider      string  `json:"provider"`
	TaskRank      int     `json:"task_rank"`
	Priority      int     `json:"priority"`
	ContextRank   int     `json:"context_rank"`
	ContextWindow int     `json:"context_window"`
	LimitSource   string  `json:"limit_source"`
	AssumedLimit  bool    `json:"assumed_limit,omitempty"`
	EstimatedCost float64 `json:"estimated_cost"`
	Reason        string  `json:"reason"`
}

type Decision struct {
	Task       string            `json:"task"`
	Selected   provider.Snapshot `json:"-"`
	Provider   string            `json:"provider"`
	Candidates []Candidate       `json:"candidates"`
	Rejected   map[string]string `json:"rejected,omitempty"`
}

type CapabilityLookup func(provider.Snapshot) (driver.Capabilities, bool)

func Select(providers []provider.Snapshot, request Request, lookup CapabilityLookup) (Decision, error) {
	if request.Task == "" {
		request.Task = TaskGeneral
	}
	decision := Decision{Task: request.Task, Rejected: make(map[string]string)}
	byName := make(map[string]provider.Snapshot, len(providers))
	for _, snapshot := range providers {
		byName[snapshot.Name] = snapshot
		capabilities, known := lookup(snapshot)
		if !known {
			decision.Rejected[snapshot.Name] = "driver capabilities are unknown"
			continue
		}
		if missing := missingCapabilities(capabilities, request.Capabilities); missing != "" {
			decision.Rejected[snapshot.Name] = "missing capabilities: " + missing
			continue
		}
		contextWindow := snapshot.ContextWindowLimit()
		if contextWindow > 0 && request.RequiredContext > contextWindow {
			decision.Rejected[snapshot.Name] = fmt.Sprintf("context window %d is below required %d", contextWindow, request.RequiredContext)
			continue
		}
		taskRank, accepted := taskRank(snapshot.Config.Routing.Tasks, request.Task)
		if !accepted {
			decision.Rejected[snapshot.Name] = fmt.Sprintf("task %s is outside configured routing tasks", request.Task)
			continue
		}
		contextRank := 1
		if contextWindow > 0 {
			contextRank = 2
		}
		if contextWindow > 0 && snapshot.Limits.Source != provider.LimitSourceUnknown && snapshot.Limits.Source != provider.LimitSourceFallback && !snapshot.Limits.Assumed {
			contextRank = 3
		}
		cost := float64(request.EstimatedInput)*snapshot.Config.Routing.InputCostPerMillion/1_000_000 +
			float64(request.EstimatedOutput)*snapshot.Config.Routing.OutputCostPerMillion/1_000_000
		decision.Candidates = append(decision.Candidates, Candidate{
			Provider: snapshot.Name, TaskRank: taskRank, Priority: snapshot.Config.Routing.Priority,
			ContextRank: contextRank, ContextWindow: contextWindow, LimitSource: string(snapshot.Limits.Source), AssumedLimit: snapshot.Limits.Assumed, EstimatedCost: cost,
			Reason: fmt.Sprintf("task_rank=%d priority=%d context=%d source=%s estimated_cost=%.8f", taskRank, snapshot.Config.Routing.Priority, contextWindow, snapshot.Limits.Source, cost),
		})
	}
	if len(decision.Candidates) == 0 {
		return decision, fmt.Errorf("no provider satisfies task %s, required context %d, and driver capabilities", request.Task, request.RequiredContext)
	}
	sort.SliceStable(decision.Candidates, func(i, j int) bool {
		left, right := decision.Candidates[i], decision.Candidates[j]
		if left.TaskRank != right.TaskRank {
			return left.TaskRank > right.TaskRank
		}
		if left.Priority != right.Priority {
			return left.Priority > right.Priority
		}
		if left.ContextRank != right.ContextRank {
			return left.ContextRank > right.ContextRank
		}
		if math.Abs(left.EstimatedCost-right.EstimatedCost) > 1e-12 {
			return left.EstimatedCost < right.EstimatedCost
		}
		if left.ContextWindow != right.ContextWindow {
			return left.ContextWindow > right.ContextWindow
		}
		return left.Provider < right.Provider
	})
	decision.Provider = decision.Candidates[0].Provider
	decision.Selected = byName[decision.Provider]
	return decision, nil
}

func Classify(prompt string) string {
	value := strings.ToLower(prompt)
	for _, rule := range []struct {
		task     string
		keywords []string
	}{
		{TaskReview, []string{"review", "audit", "inspect changes", "审查", "评审", "代码检查"}},
		{TaskDebugging, []string{"debug", "bug", "fix error", "stack trace", "修复", "报错", "故障", "错误"}},
		{TaskTesting, []string{"test", "coverage", "benchmark", "测试", "覆盖率", "基准"}},
		{TaskDocumentation, []string{"documentation", "readme", "docs", "文档", "说明书"}},
		{TaskCoding, []string{"implement", "refactor", "build", "code", "feature", "实现", "开发", "重构", "编码"}},
	} {
		for _, keyword := range rule.keywords {
			if strings.Contains(value, keyword) {
				return rule.task
			}
		}
	}
	return TaskGeneral
}

func ValidTask(task string) bool {
	switch task {
	case TaskGeneral, TaskCoding, TaskReview, TaskDebugging, TaskTesting, TaskDocumentation:
		return true
	default:
		return false
	}
}

func taskRank(tasks []string, requested string) (int, bool) {
	if len(tasks) == 0 {
		return 1, true
	}
	for _, task := range tasks {
		if task == requested {
			return 3, true
		}
	}
	for _, task := range tasks {
		if task == TaskGeneral {
			return 2, true
		}
	}
	return 0, false
}

func missingCapabilities(available, required driver.Capabilities) string {
	missing := make([]string, 0)
	checks := []struct {
		name      string
		required  bool
		available bool
	}{
		{"text_streaming", required.TextStreaming, available.TextStreaming},
		{"tool_calling", required.ToolCalling, available.ToolCalling},
		{"parallel_tools", required.ParallelTools, available.ParallelTools},
		{"reasoning", required.Reasoning, available.Reasoning},
		{"image_input", required.ImageInput, available.ImageInput},
		{"remote_session", required.RemoteSession, available.RemoteSession},
		{"hosted_web_search", required.HostedWebSearch, available.HostedWebSearch},
		{"hosted_web_fetch", required.HostedWebFetch, available.HostedWebFetch},
		{"hosted_tool_streaming", required.HostedToolStreaming, available.HostedToolStreaming},
		{"hosted_and_function_tools", required.HostedAndFunctionTools, available.HostedAndFunctionTools},
		{"search_domain_filter", required.SearchDomainFilter, available.SearchDomainFilter},
		{"search_location", required.SearchLocation, available.SearchLocation},
		{"search_usage_details", required.SearchUsageDetails, available.SearchUsageDetails},
	}
	for _, check := range checks {
		if check.required && !check.available {
			missing = append(missing, check.name)
		}
	}
	return strings.Join(missing, ",")
}
