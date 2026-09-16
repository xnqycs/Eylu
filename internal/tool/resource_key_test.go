package tool

import (
	"encoding/json"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"Eylu/internal/policy"
)

// The aliasing policy must be verifiable for every platform from any platform.
func TestResourceKeyFollowsThePlatformCasePolicy(t *testing.T) {
	tests := []struct {
		goos string
		path string
		want string
	}{
		{goos: "windows", path: "/Workspace/Sub/A.GO", want: "/workspace/sub/a.go"},
		{goos: "windows", path: "/workspace/sub/a.go", want: "/workspace/sub/a.go"},
		{goos: "windows", path: "/Workspace/Sub/A.GO/", want: "/workspace/sub/a.go"},
		{goos: "linux", path: "/Workspace/Sub/A.GO", want: "/Workspace/Sub/A.GO"},
		{goos: "darwin", path: "/workspace/sub/a.go", want: "/workspace/sub/a.go"},
		{goos: "linux", path: "/workspace/sub/", want: "/workspace/sub"},
		{goos: "linux", path: "/", want: "/"},
		{goos: "windows", path: "/", want: "/"},
		{goos: "linux", path: "  ", want: ""},
		{goos: "windows", path: ".", want: ""},
	}
	for _, test := range tests {
		if got := resourceKeyFor(test.goos, test.path); got != test.want {
			t.Errorf("resourceKeyFor(%q, %q) = %q, want %q", test.goos, test.path, got, test.want)
		}
	}
	// Case-insensitive merging is the Windows policy and only the Windows policy.
	if resourceKeyFor("linux", "/Workspace/A.GO") == resourceKeyFor("linux", "/workspace/a.go") {
		t.Fatal("POSIX paths with different case were merged")
	}
	if resourceKeyFor("windows", "/Workspace/A.GO") != resourceKeyFor("windows", "/workspace/a.go") {
		t.Fatal("Windows paths with different case were not merged")
	}
}

func TestResourceKeyKeepsVolumeRootOnWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("volume roots use host separator rules")
	}
	for _, input := range []string{`C:\`, `C:/`, `c:\`} {
		if got := resourceKey(input); got != "c:/" {
			t.Fatalf("resourceKey(%q) = %q, want the volume root key", input, got)
		}
	}
}

// A read-only shell command and a write inside the same workspace must always be
// recognized as conflicting, whatever the platform reports as the workspace path.
func TestBashTreeReadConflictsWithFileWriteInTheSameWorkspace(t *testing.T) {
	workspace := t.TempDir()
	bash, err := NewBash(workspace, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	write, err := NewWriteFile(workspace)
	if err != nil {
		t.Fatal(err)
	}
	treeRead := normalizeConcurrencySpec(bash.ClassifyConcurrency(nil, policy.Outcome{Classification: policy.CommandReadOnly}))
	fileWrite := normalizeConcurrencySpec(write.ClassifyConcurrency(json.RawMessage(`{"path":"nested/same.go"}`), policy.Outcome{}))
	if treeRead.Mode != ConcurrencyClaimed || fileWrite.Mode != ConcurrencyClaimed {
		t.Fatalf("treeRead=%#v fileWrite=%#v", treeRead, fileWrite)
	}
	if !concurrencyConflicts(treeRead, fileWrite) {
		t.Fatalf("a read-only command and a write in the same tree did not conflict: tree=%q file=%q", treeRead.Claims[0].Path, fileWrite.Claims[0].Path)
	}
	// Both tools must have produced the same root key for the workspace.
	if !strings.HasPrefix(fileWrite.Claims[0].Path, treeRead.Claims[0].Path+"/") {
		t.Fatalf("resource keys disagree: tree=%q file=%q", treeRead.Claims[0].Path, fileWrite.Claims[0].Path)
	}
}

// Aliases of one path must produce one key, and distinct paths must stay distinct.
func TestResourceKeysUnifyAliasesAndSeparateDistinctPaths(t *testing.T) {
	workspace := t.TempDir()
	write, err := NewWriteFile(workspace)
	if err != nil {
		t.Fatal(err)
	}
	claim := func(path string) string {
		spec := normalizeConcurrencySpec(write.ClassifyConcurrency(json.RawMessage(`{"path":`+strconvQuote(path)+`}`), policy.Outcome{}))
		if spec.Mode != ConcurrencyClaimed || len(spec.Claims) != 1 {
			t.Fatalf("claim for %q = %#v", path, spec)
		}
		return spec.Claims[0].Path
	}
	absolute := filepath.Join(workspace, "nested", "same.go")
	aliases := []string{
		"nested/same.go",
		"./nested/same.go",
		"nested//same.go",
		"nested/./same.go",
		absolute,
	}
	reference := claim(aliases[0])
	for _, alias := range aliases[1:] {
		if got := claim(alias); got != reference {
			t.Fatalf("alias %q produced %q, want %q", alias, got, reference)
		}
	}
	other := claim("nested/other.go")
	if other == reference {
		t.Fatal("distinct files produced the same key")
	}
	// A case-only difference is merged on Windows and kept apart elsewhere.
	upper := claim("nested/SAME.GO")
	if runtime.GOOS == "windows" {
		if upper != reference {
			t.Fatalf("case-only alias produced %q, want %q", upper, reference)
		}
	} else if upper == reference {
		t.Fatal("POSIX case-different files were merged")
	}
}

// Reading the same tree twice, and writing two different files, stay parallel.
func TestResourceClaimsAllowParallelWorkAndSeparateConflicts(t *testing.T) {
	workspace := t.TempDir()
	bash, err := NewBash(workspace, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	write, err := NewWriteFile(workspace)
	if err != nil {
		t.Fatal(err)
	}
	readClaim := func() ConcurrencySpec {
		return normalizeConcurrencySpec(bash.ClassifyConcurrency(nil, policy.Outcome{Classification: policy.CommandReadOnly}))
	}
	writeClaim := func(path string) ConcurrencySpec {
		return normalizeConcurrencySpec(write.ClassifyConcurrency(json.RawMessage(`{"path":`+strconvQuote(path)+`}`), policy.Outcome{}))
	}
	if concurrencyConflicts(readClaim(), readClaim()) {
		t.Fatal("two read-only tree claims conflicted")
	}
	if concurrencyConflicts(writeClaim("one.go"), writeClaim("two.go")) {
		t.Fatal("writes to different files conflicted")
	}
	if !concurrencyConflicts(readClaim(), writeClaim("sub/deep/file.go")) {
		t.Fatal("a tree read and a nested write did not conflict")
	}
	// A parent directory claim conflicts with a child file claim.
	index, err := NewRepositoryIndex(workspace)
	if err != nil {
		t.Fatal(err)
	}
	list := NewListDirectory(index, 0)
	directory := normalizeConcurrencySpec(list.ClassifyConcurrency(json.RawMessage(`{"path":"sub"}`), policy.Outcome{}))
	if !concurrencyConflicts(directory, writeClaim("sub/file.go")) {
		t.Fatal("a directory read and a child write did not conflict")
	}
}

// A claim whose identity cannot be established falls back to exclusive work.
func TestNormalizeConcurrencySpecFallsBackToExclusive(t *testing.T) {
	tests := []ConcurrencySpec{
		{Mode: ConcurrencyClaimed},
		{Mode: ConcurrencyClaimed, Claims: []ResourceClaim{{Kind: ResourceFile, Path: "", Access: ResourceWrite}}},
		{Mode: ConcurrencyClaimed, Claims: []ResourceClaim{{Kind: ResourceKind("device"), Path: "/dev/null", Access: ResourceWrite}}},
		{Mode: ConcurrencyClaimed, Claims: []ResourceClaim{{Kind: ResourceFile, Path: "/tmp/x", Access: ResourceAccess("append")}}},
		{Mode: ConcurrencyMode("unknown")},
	}
	for _, spec := range tests {
		if got := normalizeConcurrencySpec(spec); got.Mode != ConcurrencyExclusive {
			t.Fatalf("normalizeConcurrencySpec(%#v) = %#v, want exclusive", spec, got)
		}
	}
}

func strconvQuote(value string) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return `""`
	}
	return string(encoded)
}
