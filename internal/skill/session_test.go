package skill

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func TestSessionActivationDigestResourcesAndContentChange(t *testing.T) {
	workspace := t.TempDir()
	home := t.TempDir()
	directory := createNamedSkill(t, filepath.Join(home, ".eylu", "skills"), "review-code", "Review Go code when asked.", "FIRST BODY")
	if err := os.MkdirAll(filepath.Join(directory, "references"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "references", "guide.md"), []byte("GUIDE"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "scripts.sh"), []byte("echo ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	registry, _ := Discover(DiscoveryOptions{Workspace: workspace, Home: home})
	session := NewSession(registry, nil)
	activation, err := session.Activate("review-code", "model")
	if err != nil {
		t.Fatal(err)
	}
	if activation.Body != "FIRST BODY" || len(activation.Resources) != 2 || activation.Resources[0] != "references/guide.md" || activation.Duplicate {
		t.Fatalf("activation = %#v", activation)
	}
	duplicate, err := session.Activate("review-code", "model")
	if err != nil || !duplicate.Duplicate || duplicate.Body != "" {
		t.Fatalf("duplicate = %#v, err=%v", duplicate, err)
	}
	writeSkillEntry(t, directory, "---\nname: review-code\ndescription: Review Go code when asked.\n---\n\nSECOND BODY\n")
	changed, err := session.Activate("review-code", "model")
	if err != nil || changed.Digest == activation.Digest || changed.Body != "SECOND BODY" || changed.Duplicate {
		t.Fatalf("changed = %#v, err=%v", changed, err)
	}
}

func TestSessionResourceBoundaries(t *testing.T) {
	workspace := t.TempDir()
	home := t.TempDir()
	directory := createNamedSkill(t, filepath.Join(home, ".eylu", "skills"), "resource-skill", "Read resources when asked.", "BODY")
	if err := os.MkdirAll(filepath.Join(directory, "references"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "references", "valid.md"), []byte("VALID RESOURCE"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "references", "binary.txt"), []byte{0xff, 0xfe}, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "references", "unsupported.exe"), []byte("text"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "references", "large.md"), []byte(strings.Repeat("x", MaxResourceBytes+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	registry, _ := Discover(DiscoveryOptions{Workspace: workspace, Home: home})
	session := NewSession(registry, nil)
	if _, _, err := session.ReadResource("resource-skill", "references/valid.md"); err == nil || !strings.Contains(err.Error(), "not activated") {
		t.Fatalf("error = %v", err)
	}
	if _, err := session.Activate("resource-skill", "model"); err != nil {
		t.Fatal(err)
	}
	content, metadata, err := session.ReadResource("resource-skill", "references/valid.md")
	if err != nil || content != "VALID RESOURCE" || metadata["resource"] != "references/valid.md" {
		t.Fatalf("content=%q metadata=%#v err=%v", content, metadata, err)
	}
	for _, test := range []struct {
		path string
		want string
	}{{"../SKILL.md", "outside"}, {"references/binary.txt", "UTF-8"}, {"references/unsupported.exe", "not supported"}, {"references/large.md", "exceeds"}} {
		if _, _, err := session.ReadResource("resource-skill", test.path); err == nil || !strings.Contains(err.Error(), test.want) {
			t.Fatalf("path=%s err=%v", test.path, err)
		}
	}
	outside := filepath.Join(t.TempDir(), "outside.md")
	if err := os.WriteFile(outside, []byte("OUTSIDE"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(directory, "references", "link.md")
	if err := os.Symlink(outside, link); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("symlink privilege unavailable: %v", err)
		}
		t.Fatal(err)
	}
	if _, _, err := session.ReadResource("resource-skill", "references/link.md"); err == nil || !strings.Contains(err.Error(), "non-symlink") {
		t.Fatalf("symlink error = %v", err)
	}
}

func TestResourceListHonorsDepth(t *testing.T) {
	root := t.TempDir()
	current := root
	for depth := 1; depth <= MaxResourceDepth+1; depth++ {
		current = filepath.Join(current, fmt.Sprintf("d%d", depth))
		if err := os.Mkdir(current, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(current, "guide.md"), []byte("ok"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	resources, err := listResources(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, resource := range resources {
		if strings.Contains(resource, fmt.Sprintf("d%d", MaxResourceDepth+1)) {
			t.Fatalf("resource beyond depth limit: %s", resource)
		}
	}
}

func TestResourceListLimitAndIgnoredDirectories(t *testing.T) {
	root := t.TempDir()
	for index := 0; index <= MaxResourcesPerSkill; index++ {
		name := filepath.Join(root, fmt.Sprintf("resource-%04d.md", index))
		if err := os.WriteFile(name, []byte("ok"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(root, "node_modules"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "node_modules", "hidden.md"), []byte("hidden"), 0o600); err != nil {
		t.Fatal(err)
	}
	resources, err := listResources(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(resources) != MaxResourcesPerSkill || slices.Contains(resources, "node_modules/hidden.md") {
		t.Fatalf("resource count=%d tail=%q", len(resources), resources[len(resources)-1])
	}
}
