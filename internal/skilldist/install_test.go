package skilldist

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"Eylu/internal/config"
)

func TestRegistryInstallVerifyUpdateAndTeamLock(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	v1Archive, v1Tree := fixtureArchive(t, "version one")
	v2Archive, v2Tree := fixtureArchive(t, "version two")
	var latest atomic.Int32
	latest.Store(2)
	t.Setenv("SKILL_REGISTRY_TOKEN", "registry-secret")
	var entries []Entry
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer registry-secret" {
			writer.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch request.URL.Path {
		case "/index.json":
			count := int(latest.Load())
			_ = json.NewEncoder(writer).Encode(Index{Version: IndexVersion, Skills: entries[:count]})
		case "/demo-v1.zip":
			_, _ = writer.Write(v1Archive)
		case "/demo-v2.zip":
			_, _ = writer.Write(v2Archive)
		default:
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	entries = []Entry{
		signedEntry(t, privateKey, Entry{Name: "demo", Version: "v1.0.0", PackageURL: server.URL + "/demo-v1.zip", SHA256: digestBytes(v1Archive), TreeSHA256: v1Tree, KeyID: "release"}),
		signedEntry(t, privateKey, Entry{Name: "demo", Version: "v2.0.0", PackageURL: server.URL + "/demo-v2.zip", SHA256: digestBytes(v2Archive), TreeSHA256: v2Tree, KeyID: "release"}),
	}
	registryConfig := config.SkillRegistryConfig{
		IndexURL: server.URL + "/index.json", PublicKeys: map[string]string{"release": base64.StdEncoding.EncodeToString(publicKey)},
		TokenEnvironment: "SKILL_REGISTRY_TOKEN", TimeoutSeconds: 5,
	}
	registry := NewRegistry("official", registryConfig, nil)
	v1, err := registry.Select(context.Background(), "demo", "v1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	workspace, home := t.TempDir(), t.TempDir()
	unmanaged, _ := Target(ScopeUser, workspace, home, "demo")
	if err := os.MkdirAll(unmanaged, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(unmanaged, "SKILL.md"), []byte("manual"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Install(context.Background(), registry, v1, InstallOptions{Scope: ScopeUser, Workspace: workspace, Home: home}); err == nil || !strings.Contains(err.Error(), "unmanaged") {
		t.Fatalf("unmanaged replacement error = %v", err)
	}
	installation, err := Install(context.Background(), registry, v1, InstallOptions{Scope: ScopeProject, Workspace: workspace, Home: home})
	if err != nil {
		t.Fatal(err)
	}
	if installation.Version != "v1.0.0" || !strings.Contains(readTestFile(t, filepath.Join(installation.Path, "SKILL.md")), "version one") {
		t.Fatalf("installation = %#v", installation)
	}
	verified, err := VerifyDirectory(installation.Path, map[string]config.SkillRegistryConfig{"official": registryConfig})
	if err != nil || verified.TreeSHA256 != v1Tree {
		t.Fatalf("verified=%#v error=%v", verified, err)
	}
	manifest, err := LoadManifest(installation.Path)
	if err != nil {
		t.Fatal(err)
	}
	latestEntry, changed, err := LatestUpdate(context.Background(), registry, manifest)
	if err != nil || !changed || latestEntry.Version != "v2.0.0" {
		t.Fatalf("latest=%#v changed=%t error=%v", latestEntry, changed, err)
	}
	updated, err := Install(context.Background(), registry, latestEntry, InstallOptions{Scope: ScopeProject, Workspace: workspace, Home: home})
	if err != nil || !strings.Contains(readTestFile(t, filepath.Join(updated.Path, "SKILL.md")), "version two") {
		t.Fatalf("updated=%#v error=%v", updated, err)
	}
	if _, err := VerifyDirectory(updated.Path, map[string]config.SkillRegistryConfig{"official": registryConfig}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(updated.Path, "reference.md"), []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyDirectory(updated.Path, map[string]config.SkillRegistryConfig{"official": registryConfig}); err == nil || !strings.Contains(err.Error(), "tree digest") {
		t.Fatalf("tamper error = %v", err)
	}

	teamV1, err := Install(context.Background(), registry, v1, InstallOptions{Scope: ScopeTeam, Workspace: workspace, Home: home})
	if err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(workspace, ".eylu", "skills.lock.json")
	validLockData := readTestFile(t, lockPath)
	if err := os.WriteFile(lockPath, []byte("invalid"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Install(context.Background(), registry, latestEntry, InstallOptions{Scope: ScopeTeam, Workspace: workspace, Home: home}); err == nil || !strings.Contains(err.Error(), "lock file is invalid") {
		t.Fatalf("invalid lock error = %v", err)
	}
	if !strings.Contains(readTestFile(t, filepath.Join(teamV1.Path, "SKILL.md")), "version one") {
		t.Fatal("failed team update replaced the existing Skill")
	}
	if err := os.WriteFile(lockPath, []byte(validLockData), 0o600); err != nil {
		t.Fatal(err)
	}
	team, err := Install(context.Background(), registry, latestEntry, InstallOptions{Scope: ScopeTeam, Workspace: workspace, Home: home})
	if err != nil {
		t.Fatal(err)
	}
	lockData := readTestFile(t, lockPath)
	if !strings.Contains(lockData, `"version": "v2.0.0"`) || strings.Contains(lockData, filepath.ToSlash(workspace)) || !strings.Contains(filepath.ToSlash(team.Path), "/.agents/skills/demo") {
		t.Fatalf("team lock=%s installation=%#v", lockData, team)
	}
	userTarget, err := Target(ScopeUser, workspace, home, "demo")
	if err != nil || userTarget != filepath.Join(home, ".eylu", "skills", "demo") {
		t.Fatalf("user target=%s error=%v", userTarget, err)
	}
}

func TestEnsureSafeDirectoryRejectsSymlinkParent(t *testing.T) {
	boundary := t.TempDir()
	outside := t.TempDir()
	link := filepath.Join(boundary, "linked")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := ensureSafeDirectory(boundary, filepath.Join(link, "skills")); err == nil {
		t.Fatal("symlink installation parent was accepted")
	}
	if _, err := os.Stat(filepath.Join(outside, "skills")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("outside directory was modified: %v", err)
	}
}

func TestRegistryRejectsInvalidSignatureAndArchiveTraversal(t *testing.T) {
	publicKey, _, _ := ed25519.GenerateKey(rand.Reader)
	entry := Entry{
		Name: "demo", Version: "v1.0.0", PackageURL: "https://example.test/demo.zip",
		SHA256: strings.Repeat("0", 64), TreeSHA256: strings.Repeat("1", 64), KeyID: "release",
		Signature: base64.StdEncoding.EncodeToString(make([]byte, ed25519.SignatureSize)),
	}
	if err := validateEntry(entry, map[string]string{"release": base64.StdEncoding.EncodeToString(publicKey)}); err == nil || !strings.Contains(err.Error(), "signature") {
		t.Fatalf("error = %v", err)
	}
	var archive bytes.Buffer
	writer := zip.NewWriter(&archive)
	file, _ := writer.Create("../escape.txt")
	_, _ = file.Write([]byte("escape"))
	_ = writer.Close()
	if err := extractArchive(archive.Bytes(), t.TempDir()); err == nil || !strings.Contains(err.Error(), "invalid") {
		t.Fatalf("traversal error = %v", err)
	}
}

func fixtureArchive(t *testing.T, body string) ([]byte, string) {
	t.Helper()
	directory := t.TempDir()
	content := "---\nname: demo\ndescription: Distributed demo skill\n---\n" + body
	if err := os.WriteFile(filepath.Join(directory, "SKILL.md"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "reference.md"), []byte("reference"), 0o600); err != nil {
		t.Fatal(err)
	}
	tree, err := TreeDigest(directory)
	if err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	writer := zip.NewWriter(&archive)
	for _, name := range []string{"SKILL.md", "reference.md"} {
		entry, err := writer.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		file, _ := os.Open(filepath.Join(directory, name))
		if _, err := io.Copy(entry, file); err != nil {
			t.Fatal(err)
		}
		_ = file.Close()
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return archive.Bytes(), tree
}

func signedEntry(t *testing.T, privateKey ed25519.PrivateKey, entry Entry) Entry {
	t.Helper()
	entry.Signature = base64.StdEncoding.EncodeToString(ed25519.Sign(privateKey, SignaturePayload(entry)))
	return entry
}

func digestBytes(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func readTestFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
