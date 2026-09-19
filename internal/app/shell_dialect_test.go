package app

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// S-07 at the entry point: a shell whose command line rules the classifier does
// not model is a configuration error the user is told about, not a silent
// fallback to rules the shell does not follow.
//
// The alternative is what this replaced: EYLU_SHELL=powershell.exe made the
// policy read PowerShell with POSIX quoting and separator rules, so a line it
// classified as read-only could be a separator to the shell. A user who set that
// variable believed the classifier was reading their commands correctly, and
// nothing said otherwise.
func TestAnUnmodelledShellIsRefusedAtTheEntryPoint(t *testing.T) {
	isolateUserState(t)
	server := requestUsageServer(t)
	workspace := t.TempDir()
	t.Setenv("EYLU_API_KEY", "shell-secret")
	t.Setenv("EYLU_SHELL", "powershell.exe")

	var stdout, stderr bytes.Buffer
	code := Execute(context.Background(), requestUsageArgs(workspace, filepath.Join(workspace, "config.toml"), server.URL, "--output", "json"), strings.NewReader(""), &stdout, &stderr)
	if code != exitConfig {
		t.Fatalf("exit = %d, want the configuration exit code %d (stderr = %s)", code, exitConfig, stderr.String())
	}
	if !strings.Contains(stderr.String(), "EYLU_SHELL") {
		t.Fatalf("the error does not name the setting that caused it: %s", stderr.String())
	}
	if !strings.Contains(stderr.String(), `"code":"config_error"`) {
		t.Fatalf("the error is not reported as a configuration error: %s", stderr.String())
	}
	if strings.Contains(stdout.String(), "phase zero") {
		t.Fatal("a model request ran with an unmodelled shell")
	}
}

// The same entry point keeps working when the shell is one the classifier knows,
// including the command interpreter named by its full executable name.
func TestAModelledShellStillReachesTheModel(t *testing.T) {
	for _, configured := range []string{"", "cmd.exe", "bash"} {
		t.Run("shell="+configured, func(t *testing.T) {
			isolateUserState(t)
			server := requestUsageServer(t)
			workspace := t.TempDir()
			t.Setenv("EYLU_API_KEY", "shell-secret")
			t.Setenv("EYLU_SHELL", configured)

			var stdout, stderr bytes.Buffer
			code := Execute(context.Background(), requestUsageArgs(workspace, filepath.Join(workspace, "config.toml"), server.URL, "--output", "json"), strings.NewReader(""), &stdout, &stderr)
			if code != 0 {
				t.Fatalf("exit = %d (stderr = %s)", code, stderr.String())
			}
			// The fixture answers two rounds, so a completed request with two model
			// calls is proof the shell configuration got past the executor.
			if !strings.Contains(stdout.String(), `"stop":"completed"`) || !strings.Contains(stdout.String(), `"request_model_calls":2`) {
				t.Fatalf("the request did not reach the model: %s", stdout.String())
			}
		})
	}
}
