package app

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"Eylu/internal/agent"
	"Eylu/internal/config"
	"Eylu/internal/protocol"
	"Eylu/internal/provider"
	"Eylu/internal/skill"
	"Eylu/internal/ui"
)

// stopConsistencyCase is one provider outcome and the conclusion every output has
// to reach about it.
type stopConsistencyCase struct {
	name     string
	envelope string
	wantStop protocol.StopKind
	// wantNote is the phrase the note must contain. An empty value means the
	// request finished normally and no note may be printed at all.
	wantNote string
}

func stopConsistencyCases() []stopConsistencyCase {
	body := func(status, extra string) string {
		return `"id":"response-1","status":"` + status + `"` + extra + `,"output":[{"type":"message","content":[{"type":"output_text","text":"an answer"}]}],"usage":{"input_tokens":5,"output_tokens":3}`
	}
	return []stopConsistencyCase{
		{name: "completed", envelope: body("completed", ""), wantStop: protocol.StopCompleted},
		{
			name:     "length",
			envelope: body("incomplete", `,"incomplete_details":{"reason":"max_output_tokens"}`),
			wantStop: protocol.StopLength,
			wantNote: "stopped the response before it finished",
		},
		{
			name:     "cancelled",
			envelope: body("cancelled", ""),
			wantStop: protocol.StopCancelled,
			wantNote: "the provider reported that the response was cancelled",
		},
		{
			name:     "failed",
			envelope: body("failed", ""),
			wantStop: protocol.StopError,
			// The failure's own sentence is the note, and the CLI reports it as the
			// request error rather than as a trailing note.
			wantNote: "failed response",
		},
	}
}

// stopServer answers both transport shapes for one envelope, because the
// structured output modes do not stream and the TUI does.
func stopServer(t *testing.T, envelope string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if bytes.Contains(body, []byte(`"stream":true`)) {
			w.Header().Set("Content-Type", "text/event-stream")
			eventType := "response.completed"
			if strings.Contains(envelope, `"status":"incomplete"`) {
				eventType = "response.incomplete"
			}
			_, _ = w.Write([]byte("data: {\"type\":\"" + eventType + "\",\"response\":{" + envelope + "}}\n\n"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("{" + envelope + "}"))
	}))
	t.Cleanup(server.Close)
	return server
}

// The four outputs must reach the same conclusion about the same request: the
// structured ones carry the stop field, and the human-readable ones say the same
// sentence, taken from one function so they cannot drift apart.
func TestEveryOutputAgreesAboutWhyARequestStopped(t *testing.T) {
	for _, testCase := range stopConsistencyCases() {
		t.Run(testCase.name, func(t *testing.T) {
			isolateUserState(t)
			workspace := t.TempDir()
			t.Setenv("EYLU_API_KEY", "consistency-secret")
			server := stopServer(t, testCase.envelope)
			configPath := filepath.Join(workspace, "config.toml")
			baseArgs := []string{"--config", configPath, "--workspace", workspace, "chat", "explain", "--base-url", server.URL, "--model", "test-model"}

			t.Run("text", func(t *testing.T) {
				var stdout, stderr bytes.Buffer
				Execute(context.Background(), baseArgs, strings.NewReader(""), &stdout, &stderr)
				assertNote(t, testCase, stderr.String(), stdout.String())
			})

			t.Run("json", func(t *testing.T) {
				args := append(append([]string(nil), baseArgs...), "--output", "json")
				var stdout, stderr bytes.Buffer
				code := Execute(context.Background(), args, strings.NewReader(""), &stdout, &stderr)
				if testCase.wantStop == protocol.StopError {
					// A failed response is not a response: there is nothing to
					// serialize, so the request is reported as an error with a
					// non-zero exit. The jsonl and TUI outputs carry the same
					// conclusion through their own channel.
					if code == 0 || !strings.Contains(stderr.String(), testCase.wantNote) {
						t.Fatalf("exit=%d stderr=%q, want the failure reported", code, stderr.String())
					}
					return
				}
				if code != 0 {
					t.Fatalf("exit=%d stderr=%q", code, stderr.String())
				}
				var response protocol.ModelResponse
				if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
					t.Fatalf("decode %q: %v", stdout.String(), err)
				}
				if response.Stop != testCase.wantStop {
					t.Fatalf("stop = %q, want %q", response.Stop, testCase.wantStop)
				}
			})

			t.Run("jsonl", func(t *testing.T) {
				args := append(append([]string(nil), baseArgs...), "--output", "jsonl")
				var stdout, stderr bytes.Buffer
				Execute(context.Background(), args, strings.NewReader(""), &stdout, &stderr)
				// Every line is valid JSON, and the stop reason is carried by the
				// streamed terminal event in every case - including the failure, whose
				// conclusion reaches the stream even though there is no successful
				// response to serialize.
				want := `"stop":"` + string(testCase.wantStop) + `"`
				found := false
				for _, raw := range strings.Split(strings.TrimSpace(stdout.String()), "\n") {
					if raw == "" {
						continue
					}
					var decoded map[string]any
					if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
						t.Fatalf("decode %q: %v", raw, err)
					}
					if strings.Contains(raw, want) {
						found = true
					}
				}
				if !found {
					t.Fatalf("no line carrying %s in %q", want, stdout.String())
				}
			})

			t.Run("tui history", func(t *testing.T) {
				backend, events := tuiBackendForServer(t, workspace, configPath, server.URL)
				err := backend.Submit(context.Background(), "op-consistency", ui.Submission{Text: "explain"}, func(event ui.Event) {
					events = append(events, event)
				})
				notice := ""
				for _, event := range events {
					if event.Kind == ui.EventNotice && strings.Contains(event.Notice, testCase.wantNote) {
						notice = event.Notice
					}
					if event.Kind == ui.EventNotice && event.Error && testCase.wantNote != "" && strings.Contains(event.Notice, testCase.wantNote) {
						notice = event.Notice
					}
				}
				if testCase.wantNote == "" {
					for _, event := range events {
						if event.Kind == ui.EventNotice && strings.Contains(event.Notice, "stopped") {
							t.Fatalf("a completed request produced a stop note: %q", event.Notice)
						}
					}
					return
				}
				// The failure case reports through the request error rather than a
				// separate note, so the error text is part of the same conclusion.
				if notice == "" && err != nil {
					notice = err.Error()
				}
				if !strings.Contains(notice, testCase.wantNote) {
					t.Fatalf("the TUI said %q, want a note containing %q (events = %#v, err = %v)", notice, testCase.wantNote, events, err)
				}
			})
		})
	}
}

// assertNote checks the text output: the answer is printed, and the note is
// printed exactly when the request did not finish normally.
func assertNote(t *testing.T, testCase stopConsistencyCase, stderr, stdout string) {
	t.Helper()
	if !strings.Contains(stdout, "an answer") {
		t.Fatalf("the answer was not printed: %q", stdout)
	}
	if testCase.wantNote == "" {
		if strings.Contains(stderr, "stopped") {
			t.Fatalf("a completed request produced a stop note: %q", stderr)
		}
		return
	}
	// The failure case returns an error, which the command prints; the other cases
	// print the note.
	combined := stderr + stdout
	if !strings.Contains(combined, testCase.wantNote) {
		t.Fatalf("no statement containing %q in stderr=%q stdout=%q", testCase.wantNote, stderr, stdout)
	}
}

// tuiBackendForServer wires a TUI backend against one stub provider.
func tuiBackendForServer(t *testing.T, workspace, configPath, baseURL string) (*tuiBackend, []ui.Event) {
	t.Helper()
	cfg := config.Default()
	cfg.PermissionMode = "full"
	cfg.ActiveProvider = "work"
	cfg.Providers["work"] = config.ProviderConfig{Adapter: "openai_responses", BaseURL: baseURL, Model: "test-model"}
	manager, err := provider.NewManager(configPath, cfg, func(string, config.Config) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	registry, err := skill.Discover(skill.DiscoveryOptions{Workspace: workspace, Home: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	appRuntime := &runtime{stdin: strings.NewReader(""), stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}, workspace: workspace, trustPrompted: make(map[string]bool)}
	backend := &tuiBackend{
		runtime: appRuntime, conversation: agent.NewConversation(), manager: manager,
		skills: registry, skillSession: skill.NewSession(registry, nil),
	}
	return backend, make([]ui.Event, 0, 8)
}
