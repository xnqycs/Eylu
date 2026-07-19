package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"Eylu/internal/policy"
	"Eylu/internal/protocol"
)

const (
	maxAskQuestions        = 5
	minAskOptions          = 2
	maxAskOptions          = 4
	maxAskHeaderRunes      = 12
	maxAskQuestionRunes    = 500
	maxAskLabelRunes       = 80
	maxAskDescriptionRunes = 240
)

var ErrAskDismissed = errors.New("user dismissed questions")

type AskFunc func(context.Context, protocol.AskRequest) (protocol.AskResponse, error)

type Ask struct{ ask AskFunc }

func NewAsk(ask AskFunc) *Ask { return &Ask{ask: ask} }

func (*Ask) Definition() protocol.ToolDefinition {
	return protocol.ToolDefinition{
		Name:        "ask",
		Description: "Ask the user one to five short questions and wait for their answers. Each question has two to four choices and may allow multiple selections; the client also offers a custom answer. Use this only when the answer materially changes the work. Never request passwords, API keys, tokens, or other secrets.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"questions":{"type":"array","minItems":1,"maxItems":5,"items":{"type":"object","properties":{"id":{"type":"string","pattern":"^[a-z][a-z0-9_]*$"},"header":{"type":"string","minLength":1,"maxLength":12},"question":{"type":"string","minLength":1,"maxLength":500},"multiple":{"type":"boolean","default":false},"options":{"type":"array","minItems":2,"maxItems":4,"items":{"type":"object","properties":{"label":{"type":"string","minLength":1,"maxLength":80},"description":{"type":"string","minLength":1,"maxLength":240}},"required":["label","description"],"additionalProperties":false}}},"required":["id","header","question","options"],"additionalProperties":false}}},"required":["questions"],"additionalProperties":false}`),
	}
}

func (*Ask) Risk() policy.Risk { return policy.RiskSession }

func (*Ask) UseExecutorTimeout() bool { return false }

func (a *Ask) Execute(ctx context.Context, raw json.RawMessage) protocol.ToolResult {
	var request protocol.AskRequest
	if err := decodeStrict(raw, &request); err != nil {
		return toolError("invalid ask input: " + err.Error())
	}
	if err := validateAskQuestions(request.Questions); err != nil {
		return toolError("invalid ask input: " + err.Error())
	}
	if a == nil || a.ask == nil {
		return toolError("ask is unavailable without an interactive client")
	}
	response, err := a.ask(ctx, request)
	if err != nil {
		result := toolError("ask failed: " + err.Error())
		if errors.Is(err, ErrAskDismissed) {
			result.Metadata = map[string]any{"interrupt_request": true, "ask_dismissed": true}
		}
		return result
	}
	if err := validateAskResponse(request, &response); err != nil {
		return toolError("invalid ask response: " + err.Error())
	}
	encoded, _ := json.MarshalIndent(response, "", "  ")
	return protocol.ToolResult{Content: string(encoded)}
}

func validateAskQuestions(questions []protocol.AskQuestion) error {
	if len(questions) < 1 || len(questions) > maxAskQuestions {
		return fmt.Errorf("questions must contain 1 to %d items", maxAskQuestions)
	}
	seen := make(map[string]struct{}, len(questions))
	for index := range questions {
		question := &questions[index]
		question.ID = strings.TrimSpace(question.ID)
		question.Header = strings.TrimSpace(question.Header)
		question.Question = strings.TrimSpace(question.Question)
		if !stableIDPattern.MatchString(question.ID) {
			return fmt.Errorf("question %d has invalid id %q", index+1, question.ID)
		}
		if _, exists := seen[question.ID]; exists {
			return fmt.Errorf("question id %q is duplicated", question.ID)
		}
		seen[question.ID] = struct{}{}
		if question.Header == "" || utf8.RuneCountInString(question.Header) > maxAskHeaderRunes {
			return fmt.Errorf("question %q header must contain 1 to %d characters", question.ID, maxAskHeaderRunes)
		}
		if question.Question == "" || utf8.RuneCountInString(question.Question) > maxAskQuestionRunes {
			return fmt.Errorf("question %q text must contain 1 to %d characters", question.ID, maxAskQuestionRunes)
		}
		if len(question.Options) < minAskOptions || len(question.Options) > maxAskOptions {
			return fmt.Errorf("question %q must have %d to %d options", question.ID, minAskOptions, maxAskOptions)
		}
		labels := make(map[string]struct{}, len(question.Options))
		for optionIndex := range question.Options {
			option := &question.Options[optionIndex]
			option.Label = strings.TrimSpace(option.Label)
			option.Description = strings.TrimSpace(option.Description)
			if option.Label == "" || utf8.RuneCountInString(option.Label) > maxAskLabelRunes {
				return fmt.Errorf("question %q option %d has invalid label", question.ID, optionIndex+1)
			}
			if option.Description == "" || utf8.RuneCountInString(option.Description) > maxAskDescriptionRunes {
				return fmt.Errorf("question %q option %d has invalid description", question.ID, optionIndex+1)
			}
			key := strings.ToLower(option.Label)
			if _, exists := labels[key]; exists {
				return fmt.Errorf("question %q option label %q is duplicated", question.ID, option.Label)
			}
			labels[key] = struct{}{}
		}
	}
	return nil
}

func validateAskResponse(request protocol.AskRequest, response *protocol.AskResponse) error {
	if response.Answers == nil {
		return errors.New("answers are required")
	}
	questions := make(map[string]protocol.AskQuestion, len(request.Questions))
	for _, question := range request.Questions {
		questions[question.ID] = question
		answers, exists := response.Answers[question.ID]
		if !exists {
			return fmt.Errorf("question %q is unanswered", question.ID)
		}
		if !question.Multiple && len(answers) != 1 {
			return fmt.Errorf("question %q requires exactly one answer", question.ID)
		}
		if question.Multiple && (len(answers) < 1 || len(answers) > maxAskOptions+1) {
			return fmt.Errorf("question %q requires 1 to %d answers", question.ID, maxAskOptions+1)
		}
		seen := make(map[string]struct{}, len(answers))
		for index := range answers {
			answers[index] = strings.TrimSpace(answers[index])
			if answers[index] == "" {
				return fmt.Errorf("question %q contains an empty answer", question.ID)
			}
			key := strings.ToLower(answers[index])
			if _, duplicate := seen[key]; duplicate {
				return fmt.Errorf("question %q contains duplicate answer %q", question.ID, answers[index])
			}
			seen[key] = struct{}{}
		}
		response.Answers[question.ID] = answers
	}
	for id := range response.Answers {
		if _, exists := questions[id]; !exists {
			return fmt.Errorf("answer for unknown question %q", id)
		}
	}
	return nil
}
