package policy

import (
	"strings"
	"testing"
)

// Narrowing a setting is what has to reach a request that is already running;
// widening it must not, because a running request is never granted something it
// did not have when it started.
func TestTighteningFiresOnlyWhenASettingNarrows(t *testing.T) {
	tests := []struct {
		name     string
		previous SafetySettings
		next     SafetySettings
		want     string
	}{
		{
			name:     "full to plan",
			previous: SafetySettings{Mode: ModeFull},
			next:     SafetySettings{Mode: ModePlan},
			want:     "permission mode full -> plan",
		},
		{
			name:     "auto to manual",
			previous: SafetySettings{Mode: ModeAuto},
			next:     SafetySettings{Mode: ModeManual},
			want:     "permission mode auto -> manual",
		},
		{
			name:     "plan to manual",
			previous: SafetySettings{Mode: ModePlan},
			next:     SafetySettings{Mode: ModeManual},
			want:     "permission mode plan -> manual",
		},
		{
			name:     "wide to wide",
			previous: SafetySettings{Mode: ModeAuto},
			next:     SafetySettings{Mode: ModeFull},
		},
		{
			name:     "unchanged mode",
			previous: SafetySettings{Mode: ModePlan},
			next:     SafetySettings{Mode: ModePlan},
		},
		{
			name:     "hosted web allow to deny",
			previous: SafetySettings{Mode: ModeAuto, WebPermission: "allow"},
			next:     SafetySettings{Mode: ModeAuto, WebPermission: "deny"},
			want:     "hosted web permission allow -> deny",
		},
		{
			name:     "hosted web ask to allow is a widening",
			previous: SafetySettings{Mode: ModeAuto, WebPermission: "ask"},
			next:     SafetySettings{Mode: ModeAuto, WebPermission: "allow"},
		},
		{
			name:     "the default hosted web permission ranks as allow",
			previous: SafetySettings{Mode: ModeAuto},
			next:     SafetySettings{Mode: ModeAuto, WebPermission: "deny"},
			want:     "hosted web permission allow (default) -> deny",
		},
		{
			name:     "an unrecognized permission can only narrow",
			previous: SafetySettings{Mode: ModeAuto, WebPermission: "allow"},
			next:     SafetySettings{Mode: ModeAuto, WebPermission: "future-value"},
			want:     "hosted web permission allow -> future-value",
		},
		{
			name:     "a new denied tool",
			previous: SafetySettings{Mode: ModeAuto, DeniedTools: []string{"bash"}},
			next:     SafetySettings{Mode: ModeAuto, DeniedTools: []string{"bash", "write_file"}},
			want:     "denied tools now include write_file",
		},
		{
			name:     "removing a denied tool is a widening",
			previous: SafetySettings{Mode: ModeAuto, DeniedTools: []string{"bash", "write_file"}},
			next:     SafetySettings{Mode: ModeAuto, DeniedTools: []string{"bash"}},
		},
		{
			name:     "losing an MCP server",
			previous: SafetySettings{Mode: ModeAuto, MCPServers: []string{"files", "search"}},
			next:     SafetySettings{Mode: ModeAuto, MCPServers: []string{"files"}},
			want:     "MCP servers no longer enabled: search",
		},
		{
			name:     "gaining an MCP server is a widening",
			previous: SafetySettings{Mode: ModeAuto, MCPServers: []string{"files"}},
			next:     SafetySettings{Mode: ModeAuto, MCPServers: []string{"files", "search"}},
		},
		{
			name:     "the mode is reported first",
			previous: SafetySettings{Mode: ModeFull, WebPermission: "allow"},
			next:     SafetySettings{Mode: ModePlan, WebPermission: "deny"},
			want:     "permission mode full -> plan",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := Tightening(test.previous, test.next)
			if test.want == "" {
				if got != "" {
					t.Fatalf("tightening = %q, want none", got)
				}
				return
			}
			if !strings.Contains(got, test.want) {
				t.Fatalf("tightening = %q, want %q", got, test.want)
			}
		})
	}
}
