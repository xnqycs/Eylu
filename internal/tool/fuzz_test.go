package tool

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fuzzPaths seeds the resolver with the shapes that escape a workspace if it is
// read naively: the parent reference, the separator of the other platform, an
// absolute path, a volume-relative path, a device name, a path that is too long
// and one that ends inside a multi-byte rune.
var fuzzPaths = []string{
	"",
	".",
	"..",
	"../..",
	"../outside.txt",
	"a/../../outside.txt",
	"a/b/../c",
	"a/./b",
	"./a",
	"a//b",
	"a/",
	"/",
	"/etc/passwd",
	"//server/share/file",
	`\\server\share\file`,
	`C:\Windows\System32`,
	"C:relative",
	"C:/absolute",
	`a\..\..\outside.txt`,
	`a/b\..\..\outside.txt`,
	"sub/deeper/file.txt",
	"sub/./deeper/../file.txt",
	"escape/anything.txt",
	"escape/../escape/anything.txt",
	"CON",
	"NUL",
	"aux.txt",
	"com1",
	"trailing.",
	"trailing ",
	"nested/a/b/c/d/e/f/g/h",
	strings.Repeat("a", 512),
	strings.Repeat("../", 64) + "outside.txt",
	strings.Repeat("a/", 300) + "file.txt",
	"ünïcödé/文件.txt",
	"ünïcödé/..",
	"a\x00b",
	"\x00",
	"\n",
	" ",
	"\t",
	"%TEMP%",
	"$HOME/file",
	"~/.ssh/id_rsa",
}

// FuzzPathResolver states what must be true of every path a tool is handed.
//
// The resolver is the one place that decides whether a path is inside the
// workspace, and every tool trusts its answer, so the property is checked against
// the real filesystem rather than against the resolver's own helper: an accepted
// path is re-verified with filepath.Rel, and a path that exists is resolved once
// more through EvalSymlinks and re-verified, which is what a symlink escape would
// have to survive.
func FuzzPathResolver(f *testing.F) {
	workspace, err := os.MkdirTemp("", "eylu-fuzz-workspace-")
	if err != nil {
		f.Fatal(err)
	}
	f.Cleanup(func() { _ = os.RemoveAll(workspace) })
	outside, err := os.MkdirTemp("", "eylu-fuzz-outside-")
	if err != nil {
		f.Fatal(err)
	}
	f.Cleanup(func() { _ = os.RemoveAll(outside) })

	for _, directory := range []string{"sub/deeper", "escape"} {
		if err := os.MkdirAll(filepath.Join(workspace, filepath.FromSlash(directory)), 0o755); err != nil {
			f.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(workspace, "sub", "deeper", "file.txt"), []byte("x"), 0o600); err != nil {
		f.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("secret"), 0o600); err != nil {
		f.Fatal(err)
	}
	// A link that leaves the workspace is the case the resolver exists for. It is
	// skipped rather than failed where the platform will not create one without a
	// privilege, because the property is still worth fuzzing without it.
	if err := os.Symlink(outside, filepath.Join(workspace, "escape", "out")); err != nil {
		f.Logf("no symlink escape fixture on this platform: %v", err)
	}
	resolver, err := newPathResolver(workspace)
	if err != nil {
		f.Fatal(err)
	}

	for _, seed := range fuzzPaths {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, path string) {
		if resolved, err := resolver.lexical(path); err == nil {
			assertInside(t, "lexical", resolver.root, resolved)
		}
		if resolved, err := resolver.existing(path); err == nil {
			assertInside(t, "existing", resolver.real, resolved)
			// The accepted path exists, so it can be resolved once more through the
			// real filesystem: a symlink that leaves the workspace has to be caught
			// here even if the first check missed it.
			real, evalErr := filepath.EvalSymlinks(resolved)
			if evalErr != nil {
				t.Fatalf("existing(%q) accepted %q, which does not resolve: %v", path, resolved, evalErr)
			}
			assertInside(t, "existing (re-resolved)", resolver.real, real)
		}
		if resolved, err := resolver.writeTarget(path); err == nil {
			assertInside(t, "writeTarget", resolver.real, resolved)
		}
		if resolved, err := resolver.forWrite(context.Background(), path, false); err == nil {
			assertInside(t, "forWrite", resolver.real, resolved)
		}
		if key, err := resolver.resourcePath(path); err == nil {
			if key == "" {
				t.Fatalf("resourcePath(%q) accepted the path and produced no key", path)
			}
			assertKeyInside(t, "resourcePath", resolver.rootResourceKey(), key)
		}
	})
}

// FuzzResourceKey states that the key two aliases of one file are detected by is
// total, deterministic and idempotent, and that a key built from an absolute path
// names no parent.
//
// The parent rule is stated for absolute inputs only, because that is the domain
// the production call site passes: the resolver hands resourceKey a path it has
// already proven to be inside the workspace, and Clean leaves a relative input
// alone.
func FuzzResourceKey(f *testing.F) {
	for _, seed := range fuzzPaths {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, path string) {
		for _, goos := range []string{"windows", "linux", "darwin"} {
			key := resourceKeyFor(goos, path)
			if again := resourceKeyFor(goos, key); again != key {
				t.Fatalf("resourceKeyFor(%s, %q) = %q, which is not a fixed point (%q)", goos, path, key, again)
			}
			if goos == "windows" && key != strings.ToLower(key) {
				t.Fatalf("resourceKeyFor(%s, %q) = %q, which kept its case on a case-insensitive platform", goos, path, key)
			}
			if !filepath.IsAbs(path) {
				continue
			}
			if !filepath.IsAbs(filepath.FromSlash(key)) {
				t.Fatalf("resourceKeyFor(%s, %q) = %q, which is no longer absolute", goos, path, key)
			}
			for _, segment := range strings.Split(key, "/") {
				if segment == ".." {
					t.Fatalf("resourceKeyFor(%s, %q) = %q, which names a parent", goos, path, key)
				}
			}
		}
	})
}

// assertInside re-verifies containment with filepath.Rel rather than with the
// resolver's own helper, so a mistake in that helper cannot hide behind itself.
func assertInside(t *testing.T, what, root, resolved string) {
	t.Helper()
	relative, err := filepath.Rel(root, resolved)
	if err != nil {
		t.Fatalf("%s produced %q, which cannot be compared with %q: %v", what, resolved, root, err)
	}
	if relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		t.Fatalf("%s produced %q, which is outside %q (%s)", what, resolved, root, relative)
	}
}

// assertKeyInside re-verifies that a resource key names something inside the
// workspace key, at a path boundary rather than at a string boundary.
func assertKeyInside(t *testing.T, what, rootKey, key string) {
	t.Helper()
	if key == rootKey || strings.HasPrefix(key, rootKey+"/") {
		return
	}
	t.Fatalf("%s produced key %q, which is not inside %q", what, key, rootKey)
}
