package skill

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDiscoveryEnforcesStableActiveSkillLimit(t *testing.T) {
	builtins := make([]Skill, 0, MaxActiveSkills+1)
	for index := 0; index <= MaxActiveSkills; index++ {
		builtins = append(builtins, Skill{Name: fmt.Sprintf("skill-%03d", index), Description: "Built-in test skill."})
	}
	registry, err := Discover(DiscoveryOptions{Workspace: t.TempDir(), Home: t.TempDir(), Builtins: builtins})
	if err != nil {
		t.Fatal(err)
	}
	if got := len(registry.Active()); got != MaxActiveSkills {
		t.Fatalf("active skills = %d, want %d", got, MaxActiveSkills)
	}
	if _, ok := registry.Get("skill-200"); ok {
		t.Fatal("lexically last skill exceeded the active limit")
	}
	last := registry.Records()[len(registry.Records())-1]
	if last.Status != StatusInvalid || !strings.Contains(last.Reason, "limit") {
		t.Fatalf("last record = %#v", last)
	}
}

func TestDiscoveryDirectoryLimitAndIgnoredDirectories(t *testing.T) {
	workspace := t.TempDir()
	root := filepath.Join(workspace, ".eylu", "skills")
	for _, name := range []string{".git", "node_modules", "vendor"} {
		writeSkillEntry(t, filepath.Join(root, name), "---\nname: hidden-skill\ndescription: Hidden.\n---\n")
	}
	for index := 0; index <= MaxScannedDirectories; index++ {
		if err := os.MkdirAll(filepath.Join(root, fmt.Sprintf("directory-%04d", index)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	registry, err := Discover(DiscoveryOptions{Workspace: workspace, Home: t.TempDir(), Trust: trustedWorkspaceStore(t, workspace)})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := registry.Get("hidden-skill"); ok {
		t.Fatal("ignored directory was discovered")
	}
	foundLimit := false
	for _, record := range registry.Records() {
		foundLimit = foundLimit || strings.Contains(record.Reason, "directory scan truncated")
	}
	if !foundLimit {
		t.Fatalf("records = %#v", registry.Records())
	}
}

func TestDiscoveryPrecedenceTrustCatalogAndDiagnostics(t *testing.T) {
	workspace := t.TempDir()
	home := t.TempDir()
	trust, err := OpenTrustStore(filepath.Join(t.TempDir(), "trust.json"))
	if err != nil {
		t.Fatal(err)
	}
	createNamedSkill(t, filepath.Join(home, ".agents", "skills"), "shared-skill", "user agents", "USER AGENTS BODY")
	createNamedSkill(t, filepath.Join(home, ".eylu", "skills"), "shared-skill", "user eylu", "USER EYLU BODY")
	createNamedSkill(t, filepath.Join(workspace, ".agents", "skills"), "shared-skill", "project agents", "PROJECT AGENTS BODY")
	createNamedSkill(t, filepath.Join(workspace, ".eylu", "skills"), "shared-skill", "project eylu", "PROJECT EYLU BODY")
	writeSkillEntry(t, filepath.Join(home, ".eylu", "skills", "invalid"), "---\nname: INVALID\ndescription: bad\n---\n")

	registry, err := Discover(DiscoveryOptions{Workspace: workspace, Home: home, Trust: trust})
	if err != nil {
		t.Fatal(err)
	}
	active, ok := registry.Get("shared-skill")
	if !ok || active.Source != SourceUserEylu || active.Body != "USER EYLU BODY" {
		t.Fatalf("active = %#v", active)
	}
	untrusted := 0
	invalid := 0
	for _, record := range registry.Records() {
		if record.Status == StatusUntrusted {
			untrusted++
		}
		if record.Status == StatusInvalid {
			invalid++
		}
	}
	if untrusted != 2 || invalid != 1 {
		t.Fatalf("records = %#v", registry.Records())
	}
	catalog := registry.Catalog()
	if !strings.Contains(catalog, "shared-skill") || !strings.Contains(catalog, "user eylu") || strings.Contains(catalog, "USER EYLU BODY") {
		t.Fatalf("catalog = %s", catalog)
	}

	if err := trust.Trust(workspace); err != nil {
		t.Fatal(err)
	}
	registry, err = Discover(DiscoveryOptions{Workspace: workspace, Home: home, Trust: trust})
	if err != nil {
		t.Fatal(err)
	}
	active, ok = registry.Get("shared-skill")
	if !ok || active.Source != SourceProjectEylu || active.Body != "PROJECT EYLU BODY" {
		t.Fatalf("trusted active = %#v", active)
	}
	shadowed := 0
	for _, record := range registry.Records() {
		if record.Skill.Name == "shared-skill" && record.Status == StatusShadowed && record.ShadowedBy != "" {
			shadowed++
		}
	}
	if shadowed != 3 {
		t.Fatalf("records = %#v", registry.Records())
	}
}

func TestTrustStorePersistsAndRevokes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "trust.json")
	workspace := t.TempDir()
	store, _ := OpenTrustStore(path)
	if store.IsTrusted(workspace) {
		t.Fatal("workspace starts trusted")
	}
	if err := store.Trust(workspace); err != nil {
		t.Fatal(err)
	}
	reloaded, err := OpenTrustStore(path)
	if err != nil || !reloaded.IsTrusted(workspace) {
		t.Fatalf("reloaded=%#v err=%v", reloaded, err)
	}
	if err := reloaded.Revoke(workspace); err != nil {
		t.Fatal(err)
	}
	again, _ := OpenTrustStore(path)
	if again.IsTrusted(workspace) {
		t.Fatal("workspace trust survived revoke")
	}
}

func TestCatalogNormalizesMultilineDescriptions(t *testing.T) {
	registry := newRegistry([]Record{{Skill: Skill{Name: "demo", Description: "first line\n  second line", Source: SourceBuiltin}, Status: StatusActive}}, nil)
	catalog := registry.Catalog()
	if !strings.Contains(catalog, "first line second line") || strings.Count(catalog, "\n") != 2 {
		t.Fatalf("catalog = %q", catalog)
	}
}

func createNamedSkill(t *testing.T, root, name, description, body string) string {
	t.Helper()
	directory := filepath.Join(root, name)
	writeSkillEntry(t, directory, "---\nname: "+name+"\ndescription: "+description+"\n---\n\n"+body+"\n")
	return directory
}

func trustedWorkspaceStore(t *testing.T, workspace string) *TrustStore {
	t.Helper()
	store, err := OpenTrustStore(filepath.Join(t.TempDir(), "trust.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Trust(workspace); err != nil {
		t.Fatal(err)
	}
	return store
}
