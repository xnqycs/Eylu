package openai_responses

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"

	"Eylu/internal/driver"
	"Eylu/internal/protocol"
)

type responseStreamEvent struct {
	Type        string             `json:"type"`
	Delta       string             `json:"delta"`
	Arguments   string             `json:"arguments"`
	ItemID      string             `json:"item_id"`
	OutputIndex int                `json:"output_index"`
	Query       string             `json:"query"`
	Queries     []string           `json:"queries"`
	Action      responseWebAction  `json:"action"`
	Item        responseItem       `json:"item"`
	Annotation  responseAnnotation `json:"annotation"`
	Response    responseBody       `json:"response"`
	Error       *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

type callAccumulator struct {
	ID        string
	Name      string
	Arguments strings.Builder
	Deltas    driver.StreamDeltaBuffer
}

func (d *Driver) readStream(ctx context.Context, body io.Reader, emit driver.EmitFunc) (protocol.ModelResponse, error) {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 64<<10), 8<<20)
	var data strings.Builder
	text := strings.Builder{}
	calls := make(map[int]*callAccumulator)
	startedWeb := make(map[string]bool)
	startedWebKinds := make(map[string]protocol.ToolKind)
	startedWebOrder := make([]string, 0)
	webActivities := make(map[string]protocol.WebActivity)
	terminalWeb := make(map[string]bool)
	streamCitations := make([]protocol.URLCitation, 0)
	var final *protocol.ModelResponse
	completed := false
	finalPartsEmitted := false
	emitToolDelta := func(index int, call *callAccumulator, delta string, done bool) error {
		if emit == nil || call == nil {
			return nil
		}
		update := &protocol.ToolCallDelta{OutputIndex: index, ID: call.ID, Name: call.Name, Delta: delta, Done: done}
		if done {
			update.Arguments = call.Arguments.String()
		}
		return emit(protocol.ModelEvent{Kind: protocol.EventToolCallDelta, ToolCallDelta: update})
	}
	emitWebStart := func(activity protocol.WebActivity) error {
		if activity.CallID == "" {
			return nil
		}
		activity = mergeWebActivity(webActivities[activity.CallID], activity)
		activity.Status = protocol.WebStatusRunning
		webActivities[activity.CallID] = activity
		if startedWeb[activity.CallID] {
			return nil
		}
		startedWeb[activity.CallID] = true
		startedWebKinds[activity.CallID] = activity.Kind
		startedWebOrder = append(startedWebOrder, activity.CallID)
		kind := protocol.EventWebSearchStarted
		if activity.Kind == protocol.ToolWebFetch {
			kind = protocol.EventWebFetchStarted
		}
		if emit == nil {
			return nil
		}
		return emit(protocol.ModelEvent{Kind: kind, WebActivity: &activity})
	}
	emitWebTerminal := func(callID string, kind protocol.ToolKind, raw string) error {
		if callID == "" || terminalWeb[callID] {
			return nil
		}
		activity, ok := webActivities[callID]
		if !ok {
			activity = protocol.WebActivity{CallID: callID, Kind: kind, Action: "search"}
			if kind == protocol.ToolWebFetch {
				activity.Action = "open_page"
			}
			if err := emitWebStart(activity); err != nil {
				return err
			}
			activity = webActivities[callID]
		}
		activity.Status = protocol.WebStatusCompleted
		if len(activity.RawProviderResponse) == 0 && raw != "" {
			activity.RawProviderResponse = boundedRaw([]byte(raw))
			activity.RawTruncated = len(raw) > len(activity.RawProviderResponse)
		}
		webActivities[callID] = activity
		terminalWeb[callID] = true
		eventKind := protocol.EventWebSearchCompleted
		if activity.Kind == protocol.ToolWebFetch {
			eventKind = protocol.EventWebFetchCompleted
		}
		if emit == nil {
			return nil
		}
		return emit(protocol.ModelEvent{Kind: eventKind, WebActivity: &activity})
	}
	dispatch := func() error {
		payload := strings.TrimSpace(data.String())
		data.Reset()
		if payload == "" {
			return nil
		}
		if payload == "[DONE]" {
			completed = true
			return nil
		}
		var event responseStreamEvent
		if err := json.Unmarshal([]byte(payload), &event); err != nil {
			return &protocol.Error{Code: protocol.ErrProtocol, Message: "decode Responses stream event", Cause: err}
		}
		switch event.Type {
		case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
			if emit != nil && event.Delta != "" {
				return emit(protocol.ModelEvent{Kind: protocol.EventReasoningDelta, Delta: event.Delta})
			}
		case "response.output_text.delta":
			text.WriteString(event.Delta)
			if emit != nil && event.Delta != "" {
				return emit(protocol.ModelEvent{Kind: protocol.EventTextDelta, Delta: event.Delta})
			}
		case "response.output_item.added":
			if event.Item.Type == "function_call" {
				call := &callAccumulator{ID: event.Item.CallID, Name: event.Item.Name}
				if call.ID == "" {
					call.ID = event.Item.ID
				}
				call.Arguments.WriteString(event.Item.Arguments)
				calls[event.OutputIndex] = call
				if err := emitToolDelta(event.OutputIndex, call, "", false); err != nil {
					return err
				}
			} else if strings.Contains(event.Item.Type, "web_search_call") || strings.Contains(event.Item.Type, "web_fetch_call") {
				activity := webActivityFromItem(event.Item, nil)
				if err := emitWebStart(activity); err != nil {
					return err
				}
			}
		case "response.output_item.done":
			if strings.Contains(event.Item.Type, "web_search_call") || strings.Contains(event.Item.Type, "web_fetch_call") {
				activity := webActivityFromItem(event.Item, nil)
				if activity.CallID == "" {
					activity.CallID = event.ItemID
				}
				activity = mergeWebActivity(webActivities[activity.CallID], activity)
				activity.Status = protocol.WebStatusCompleted
				webActivities[activity.CallID] = activity
				if !startedWeb[activity.CallID] {
					if err := emitWebStart(activity); err != nil {
						return err
					}
				}
				if emit != nil && !terminalWeb[activity.CallID] {
					updateKind := protocol.EventWebSearchUpdated
					if activity.Kind == protocol.ToolWebFetch {
						updateKind = protocol.EventWebFetchUpdated
					}
					if err := emit(protocol.ModelEvent{Kind: updateKind, WebActivity: &activity}); err != nil {
						return err
					}
				}
			}
		case "response.web_search_call.in_progress", "response.web_search_call.searching":
			wasStarted := startedWeb[event.ItemID]
			update := webActivityFromStreamEvent(event, protocol.ToolWebSearch)
			activity := mergeWebActivity(webActivities[event.ItemID], update)
			if err := emitWebStart(activity); err != nil {
				return err
			}
			if wasStarted && hasWebActivityDetail(update) && emit != nil {
				activity = webActivities[event.ItemID]
				if err := emit(protocol.ModelEvent{Kind: protocol.EventWebSearchUpdated, WebActivity: &activity}); err != nil {
					return err
				}
			}
		case "response.web_fetch_call.in_progress", "response.web_fetch_call.fetching":
			wasStarted := startedWeb[event.ItemID]
			update := webActivityFromStreamEvent(event, protocol.ToolWebFetch)
			activity := mergeWebActivity(webActivities[event.ItemID], update)
			if err := emitWebStart(activity); err != nil {
				return err
			}
			if wasStarted && hasWebActivityDetail(update) && emit != nil {
				activity = webActivities[event.ItemID]
				if err := emit(protocol.ModelEvent{Kind: protocol.EventWebFetchUpdated, WebActivity: &activity}); err != nil {
					return err
				}
			}
		case "response.output_text.annotation.added":
			if event.Annotation.Type == "url_citation" && event.Annotation.URL != "" {
				callID := lastString(startedWebOrder)
				streamCitations = appendUniqueCitation(streamCitations, protocol.URLCitation{
					CallID: callID, URL: event.Annotation.URL, Title: event.Annotation.Title,
					StartIndex: event.Annotation.StartIndex, EndIndex: event.Annotation.EndIndex,
				})
			}
		case "response.function_call_arguments.delta":
			if call := calls[event.OutputIndex]; call != nil {
				call.Arguments.WriteString(event.Delta)
				if batch, ready := call.Deltas.Push(event.Delta, time.Now()); ready {
					if err := emitToolDelta(event.OutputIndex, call, batch, false); err != nil {
						return err
					}
				}
			}
		case "response.function_call_arguments.done":
			if call := calls[event.OutputIndex]; call != nil {
				if batch := call.Deltas.Flush(); batch != "" {
					if err := emitToolDelta(event.OutputIndex, call, batch, false); err != nil {
						return err
					}
				}
				arguments := event.Arguments
				if arguments == "" {
					arguments = event.Item.Arguments
				}
				if arguments != "" {
					call.Arguments.Reset()
					call.Arguments.WriteString(arguments)
				}
				if err := emitToolDelta(event.OutputIndex, call, "", true); err != nil {
					return err
				}
			}
		case "response.completed":
			converted := convertResponse(event.Response)
			streamCitations = collectStreamWebState(&converted, webActivities, startedWebOrder, streamCitations)
			for _, callID := range startedWebOrder {
				if terminalWeb[callID] {
					continue
				}
				if err := emitWebTerminal(callID, startedWebKinds[callID], ""); err != nil {
					return err
				}
			}
			mergeStreamWebActivities(&converted, webActivities, startedWebOrder)
			mergeStreamCitations(&converted, streamCitations)
			finalText := responseText(converted)
			emittedText := text.String()
			missingText := ""
			if strings.HasPrefix(finalText, emittedText) {
				missingText = strings.TrimPrefix(finalText, emittedText)
			} else if emittedText == "" {
				missingText = finalText
			}
			if missingText != "" {
				text.WriteString(missingText)
				if emit != nil {
					if err := emit(protocol.ModelEvent{Kind: protocol.EventTextDelta, Delta: missingText}); err != nil {
						return err
					}
				}
			}
			if emit != nil {
				if err := emitResponseParts(converted, emit, startedWeb, terminalWeb, false); err != nil {
					return err
				}
				finalPartsEmitted = true
			}
			final = &converted
			completed = true
		case "response.failed", "error":
			message := "Responses stream failed"
			if event.Error != nil && event.Error.Message != "" {
				message = event.Error.Message
			} else if event.Response.Error != nil && event.Response.Error.Message != "" {
				message = event.Response.Error.Message
			}
			if err := emitWebFailures(emit, startedWebKinds, startedWebOrder, terminalWeb, message); err != nil {
				return err
			}
			return protocol.ClassifyProviderMessage(message)
		}
		return nil
	}
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if err := dispatch(); err != nil {
				return protocol.ModelResponse{}, err
			}
			continue
		}
		if strings.HasPrefix(line, "data:") {
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	if data.Len() > 0 {
		if err := dispatch(); err != nil {
			return protocol.ModelResponse{}, err
		}
	}
	if err := scanner.Err(); err != nil {
		_ = emitWebFailures(emit, startedWebKinds, startedWebOrder, terminalWeb, err.Error())
		if ctx.Err() != nil {
			return protocol.ModelResponse{}, mapTransportError(ctx, ctx.Err())
		}
		return protocol.ModelResponse{}, &protocol.Error{Code: protocol.ErrNetwork, Message: "read Responses stream", Retryable: true, Cause: err}
	}
	if !completed {
		_ = emitWebFailures(emit, startedWebKinds, startedWebOrder, terminalWeb, "Responses stream ended before completion")
		return protocol.ModelResponse{}, &protocol.Error{Code: protocol.ErrNetwork, Message: "Responses stream ended before completion", Retryable: true}
	}
	if final == nil {
		built := protocol.ModelResponse{Turn: protocol.Turn{ID: uuid.NewString(), Role: protocol.RoleAgent, CreatedAt: time.Now().UTC()}, Stop: protocol.StopCompleted}
		if text.Len() > 0 {
			built.Turn.Parts = append(built.Turn.Parts, protocol.Part{Kind: protocol.PartText, Text: text.String()})
		}
		for index := 0; index < len(calls); index++ {
			call := calls[index]
			if call == nil {
				continue
			}
			toolCall := protocol.ToolCall{ID: call.ID, Name: call.Name, Arguments: json.RawMessage(call.Arguments.String())}
			built.Turn.Parts = append(built.Turn.Parts, protocol.Part{Kind: protocol.PartToolCall, ToolCall: &toolCall})
			built.Stop = protocol.StopToolUse
		}
		final = &built
	}
	streamCitations = collectStreamWebState(final, webActivities, startedWebOrder, streamCitations)
	for _, callID := range startedWebOrder {
		if terminalWeb[callID] {
			continue
		}
		if err := emitWebTerminal(callID, startedWebKinds[callID], ""); err != nil {
			return protocol.ModelResponse{}, err
		}
	}
	mergeStreamWebActivities(final, webActivities, startedWebOrder)
	mergeStreamCitations(final, streamCitations)
	if len(final.Turn.Parts) == 0 {
		return protocol.ModelResponse{}, &protocol.Error{Code: protocol.ErrProtocol, Message: "provider stream returned no text or tool calls"}
	}
	if emit != nil {
		if !finalPartsEmitted {
			if err := emitResponseParts(*final, emit, startedWeb, terminalWeb, false); err != nil {
				return protocol.ModelResponse{}, err
			}
		}
		if err := emit(protocol.ModelEvent{Kind: protocol.EventUsage, Usage: &final.Usage}); err != nil {
			return protocol.ModelResponse{}, err
		}
	}
	return *final, nil
}

func emitWebFailures(emit driver.EmitFunc, started map[string]protocol.ToolKind, order []string, terminal map[string]bool, message string) error {
	if emit == nil {
		return nil
	}
	for _, callID := range order {
		if terminal[callID] {
			continue
		}
		toolKind := started[callID]
		eventKind := protocol.EventWebSearchCompleted
		if toolKind == protocol.ToolWebFetch {
			eventKind = protocol.EventWebFetchCompleted
		}
		activity := protocol.WebActivity{CallID: callID, Kind: toolKind, Status: protocol.WebStatusError, Error: message}
		if err := emit(protocol.ModelEvent{Kind: eventKind, WebActivity: &activity}); err != nil {
			return err
		}
	}
	return nil
}

func mergeStreamWebActivities(response *protocol.ModelResponse, activities map[string]protocol.WebActivity, order []string) {
	if response == nil || len(activities) == 0 {
		return
	}
	existing := make(map[string]bool, len(activities))
	for index, part := range response.Turn.Parts {
		if part.Kind == protocol.PartWebActivity && part.WebActivity != nil {
			existing[part.WebActivity.CallID] = true
			if streamed, ok := activities[part.WebActivity.CallID]; ok {
				merged := mergeWebActivity(*part.WebActivity, streamed)
				response.Turn.Parts[index].WebActivity = &merged
			}
		}
	}
	webParts := make([]protocol.Part, 0, len(order))
	for _, callID := range order {
		if existing[callID] {
			continue
		}
		activity := activities[callID]
		if activity.Status == protocol.WebStatusRunning || activity.Status == protocol.WebStatusPending || activity.Status == "" {
			activity.Status = protocol.WebStatusCompleted
		}
		webParts = append(webParts, protocol.Part{Kind: protocol.PartWebActivity, WebActivity: &activity})
	}
	response.Turn.Parts = append(webParts, response.Turn.Parts...)
}

func mergeWebActivity(current, update protocol.WebActivity) protocol.WebActivity {
	if current.CallID == "" {
		current.CallID = update.CallID
	}
	if update.Kind != "" {
		current.Kind = update.Kind
	}
	if update.Query != "" {
		current.Query = update.Query
	}
	if len(update.Queries) > 0 {
		current.Queries = append([]string(nil), update.Queries...)
	}
	if update.URL != "" {
		current.URL = update.URL
	}
	if update.Pattern != "" {
		current.Pattern = update.Pattern
	}
	if update.Action != "" {
		current.Action = update.Action
	}
	if update.Status != "" {
		current.Status = update.Status
	}
	for _, source := range update.Sources {
		current.Sources = appendUniqueSource(current.Sources, source)
	}
	if len(update.RawProviderResponse) > 0 {
		current.RawProviderResponse = append(json.RawMessage(nil), update.RawProviderResponse...)
		current.RawTruncated = update.RawTruncated
	}
	if update.Error != "" {
		current.Error = update.Error
	}
	return current
}

func webActivityFromStreamEvent(event responseStreamEvent, kind protocol.ToolKind) protocol.WebActivity {
	action := event.Action
	if action.Query == "" {
		action.Query = event.Query
	}
	if len(action.Queries) == 0 {
		action.Queries = event.Queries
	}
	activity := protocol.WebActivity{
		CallID: event.ItemID, Kind: kind, Query: action.Query, Queries: cleanWebQueries(action.Queries),
		URL: action.URL, Pattern: action.Pattern, Action: action.Type, Status: protocol.WebStatusRunning,
	}
	if activity.Action == "" {
		activity.Action = "search"
		if kind == protocol.ToolWebFetch {
			activity.Action = "open_page"
		}
	}
	return activity
}

func hasWebActivityDetail(activity protocol.WebActivity) bool {
	return activity.Query != "" || len(activity.Queries) > 0 || activity.URL != "" || activity.Pattern != ""
}

func collectStreamWebState(response *protocol.ModelResponse, activities map[string]protocol.WebActivity, order []string, citations []protocol.URLCitation) []protocol.URLCitation {
	if response == nil {
		return citations
	}
	defaultCallID := lastString(order)
	for _, part := range response.Turn.Parts {
		switch {
		case part.Kind == protocol.PartWebActivity && part.WebActivity != nil:
			activities[part.WebActivity.CallID] = mergeWebActivity(activities[part.WebActivity.CallID], *part.WebActivity)
		case part.Kind == protocol.PartCitation && part.Citation != nil:
			citation := *part.Citation
			if citation.CallID == "" {
				citation.CallID = defaultCallID
			}
			citations = appendUniqueCitation(citations, citation)
		}
	}
	if len(citations) == 0 && defaultCallID != "" {
		citations = append(citations, markdownCitations(responseText(*response), defaultCallID)...)
	}
	for _, citation := range citations {
		callID := citation.CallID
		if callID == "" {
			callID = defaultCallID
		}
		activity, ok := activities[callID]
		if !ok || citation.URL == "" {
			continue
		}
		activity.Sources = appendUniqueSource(activity.Sources, protocol.WebSource{URL: citation.URL, Title: citation.Title})
		activities[callID] = activity
	}
	return citations
}

func mergeStreamCitations(response *protocol.ModelResponse, citations []protocol.URLCitation) {
	if response == nil || len(citations) == 0 {
		return
	}
	existing := make(map[string]bool)
	for _, part := range response.Turn.Parts {
		if part.Kind == protocol.PartCitation && part.Citation != nil {
			existing[citationKey(*part.Citation)] = true
		}
	}
	for _, citation := range citations {
		if existing[citationKey(citation)] {
			continue
		}
		copy := citation
		response.Turn.Parts = append(response.Turn.Parts, protocol.Part{Kind: protocol.PartCitation, Citation: &copy})
	}
}

func appendUniqueSource(sources []protocol.WebSource, source protocol.WebSource) []protocol.WebSource {
	if strings.TrimSpace(source.URL) == "" {
		return sources
	}
	for _, current := range sources {
		if current.URL == source.URL {
			return sources
		}
	}
	return append(sources, source)
}

func appendUniqueCitation(citations []protocol.URLCitation, citation protocol.URLCitation) []protocol.URLCitation {
	if strings.TrimSpace(citation.URL) == "" {
		return citations
	}
	key := citationKey(citation)
	for _, current := range citations {
		if citationKey(current) == key {
			return citations
		}
	}
	return append(citations, citation)
}

func citationKey(citation protocol.URLCitation) string {
	return citation.CallID + "\x00" + citation.URL
}

func lastString(values []string) string {
	if len(values) == 0 {
		return ""
	}
	return values[len(values)-1]
}

var markdownLink = regexp.MustCompile(`\[([^\]]+)\]\((https?://[^\s)]+)\)`)

func markdownCitations(text, callID string) []protocol.URLCitation {
	matches := markdownLink.FindAllStringSubmatch(text, -1)
	result := make([]protocol.URLCitation, 0, len(matches))
	for _, match := range matches {
		result = appendUniqueCitation(result, protocol.URLCitation{CallID: callID, Title: match[1], URL: match[2]})
	}
	return result
}

func responseText(response protocol.ModelResponse) string {
	var text strings.Builder
	for _, part := range response.Turn.Parts {
		if part.Kind == protocol.PartText {
			text.WriteString(part.Text)
		}
	}
	return text.String()
}
