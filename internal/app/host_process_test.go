package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"

	"Eylu/internal/host"
	"Eylu/internal/hostmock"
)

func TestHostChildProcess(t *testing.T) {
	if os.Getenv("EYLU_HOST_CHILD") != "1" {
		return
	}
	os.Exit(Execute(context.Background(), []string{"serve", "--transport", "stdio", "--protocol", "bastion-host/1.0"}, os.Stdin, os.Stdout, os.Stderr))
}

func TestC14ServeStartupErrorsStayOnStderr(t *testing.T) {
	for _, args := range [][]string{{"serve", "--output", "json"}, {"serve", "--output", "json", "--unknown"}, {"serve", "--output", "json", "unexpected"}, {"serve", "--transport", "tcp"}} {
		var out, diagnostics bytes.Buffer
		if Execute(context.Background(), args, strings.NewReader(""), &out, &diagnostics) == 0 || out.Len() != 0 || diagnostics.Len() == 0 {
			t.Fatalf("startup output for %v: stdout=%q stderr=%q", args, out.String(), diagnostics.String())
		}
	}
}

func TestC11NativeKillAtDurabilityBoundaries(t *testing.T) {
	for _, boundary := range []string{"user_committed", "tool_prepared", "tool_received"} {
		t.Run(boundary, func(t *testing.T) {
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), hostmock.DefaultTimeout)
			defer cancel()
			o := hostmock.Options{Engine: executable, Arguments: []string{"-test.run=^TestHostChildProcess$"}, Env: []string{"EYLU_HOST_CHILD=1"}, LedgerPath: filepath.Join(t.TempDir(), "ledger.json"), CrashAt: boundary}
			if _, err = hostmock.Run(ctx, o); !errors.Is(err, hostmock.ErrInjectedCrash) {
				t.Fatalf("missing injected crash: %v", err)
			}
			data, err := os.ReadFile(o.LedgerPath)
			if err != nil {
				t.Fatal(err)
			}
			var saved struct {
				State host.State `json:"state"`
			}
			if err = json.Unmarshal(data, &saved); err != nil {
				t.Fatal(err)
			}
			if len(saved.State.Messages) == 0 || saved.State.Messages[0].Message.Parts[0].Text != "Inspect the synthetic target" {
				t.Fatal("crash lost user input")
			}
			o.CrashAt = ""
			result, err := hostmock.Run(ctx, o)
			if boundary == "tool_received" {
				if err == nil || !strings.Contains(err.Error(), "reconciliation remains unresolved") {
					t.Fatalf("unknown operation was resumed: %+v, %v", result, err)
				}
			} else if err != nil || result.Run.Status != "completed" {
				t.Fatalf("safe new run failed: %+v, %v", result, err)
			}
		})
	}
}

func TestC01C16NativeHostProcessIsolationRestartAndInterrupt(t *testing.T) {
	root := t.TempDir()
	poison := []byte("THIS IS INVALID CONFIG; provider secret=DO-NOT-LOG\n")
	if err := os.MkdirAll(filepath.Join(root, ".eylu"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".eylu", "config.toml"), poison, 0600); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), hostmock.DefaultTimeout)
	defer cancel()
	var diagnostics bytes.Buffer
	o := hostmock.Options{Engine: executable, Arguments: []string{"-test.run=^TestHostChildProcess$"}, LedgerPath: filepath.Join(root, "ledger.json"), Diagnostics: &diagnostics, Env: []string{"EYLU_HOST_CHILD=1", "HOME=" + root, "USERPROFILE=" + root, "OPENAI_API_KEY=DO-NOT-LOG", "ANTHROPIC_API_KEY=DO-NOT-LOG", "OPENAI_BASE_URL=http://127.0.0.1:1/forbidden", "SHELL=/missing-shell", "COMSPEC=/missing-shell", "PATH="}}
	o.WorkingDir = root
	first, err := hostmock.Run(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	if first.Run.Status != "completed" || first.Run.ModelCalls != 2 {
		t.Fatalf("%s native loop: %+v", goruntime.GOOS, first)
	}
	o.Interrupt = true
	second, err := hostmock.Run(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	if second.Revision <= first.Revision || second.EventSeq <= first.EventSeq || second.Run.Status != "interrupted" || len(second.Run.UnresolvedRequestIDs) != 0 {
		t.Fatalf("%s native restore/interrupt: %+v", goruntime.GOOS, second)
	}
	if diagnostics.Len() != 0 {
		t.Fatalf("host leaked diagnostics: %s", diagnostics.String())
	}
	t.Logf("%s/%s native startup, bidirectional callbacks, restart, interrupt and exit passed; revision %d -> %d, event_seq %d -> %d", goruntime.GOOS, goruntime.GOARCH, first.Revision, second.Revision, first.EventSeq, second.EventSeq)
}
