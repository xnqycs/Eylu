package tool

import (
	"encoding/json"
	"runtime"
	"strings"
	"testing"

	"Eylu/internal/policy"
)

// A name that differs from a parent reference only by surrounding whitespace is a
// different name, and its key has to say so.
//
// This is what the Linux leg of the fuzz gate found. resourceKey trimmed the path
// it was given before cleaning it, so `".. "` became `".."`: a path naming a file
// inside the workspace produced the key of the workspace's *parent*. A write to
// that path would then have claimed a key that never conflicts with the tree it is
// inside, which is a conflict the coordinator would never see - the one direction
// the key exists to prevent. Only the blank check trims now.
func TestResourceKeyKeepsWhitespaceThatIsPartOfTheName(t *testing.T) {
	// The key-level rule is the same under every platform policy, because cleaning
	// keeps a space that is part of an element.
	for _, goos := range []string{"windows", "linux", "darwin"} {
		for _, testCase := range []struct{ path, want string }{
			{"/a/.. ", "/a/.. "},
			{"/a/ ..", "/a/ .."},
			{"/a/. ", "/a/. "},
			{"/a/a b", "/a/a b"},
			{"/a/space\t", "/a/space\t"},
		} {
			if got := resourceKeyFor(goos, testCase.path); got != testCase.want {
				t.Fatalf("resourceKeyFor(%s, %q) = %q, want %q", goos, testCase.path, got, testCase.want)
			}
		}
		// The two spellings are one key apart, which is the whole point: a name that
		// differs from a parent reference by whitespace is not that parent.
		if resourceKeyFor(goos, "/a/.. ") == resourceKeyFor(goos, "/a/..") {
			t.Fatalf("%s: %q and %q produced one key", goos, "/a/.. ", "/a/..")
		}
		// Blank still names nothing, and so does the current directory.
		if got := resourceKeyFor(goos, "  "); got != "" {
			t.Fatalf("%s: a blank path produced %q", goos, got)
		}
		if got := resourceKeyFor(goos, " /a"); got != " /a" {
			t.Fatalf("%s: a leading space was trimmed away: %q", goos, got)
		}
		if got := resourceKeyFor(goos, "."); got != "" {
			t.Fatalf("%s: the current directory produced %q", goos, got)
		}
	}

	// The resolver is where the difference reached production. On Windows it cannot:
	// the Win32 path API strips a trailing space when it resolves a name, so `".. "`
	// and `".."` are the same place there and the claim lands on the workspace root.
	// That is the safe direction - it over-serializes instead of missing a conflict -
	// so it is asserted rather than worked around.
	workspace := t.TempDir()
	resolver, err := newPathResolver(workspace)
	if err != nil {
		t.Fatal(err)
	}
	rootKey := resolver.rootResourceKey()
	key, err := resolver.resourcePath(".. ")
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" {
		if key != rootKey {
			t.Fatalf("resourcePath(%q) = %q, want the workspace's own key on this platform", ".. ", key)
		}
	} else if !strings.HasPrefix(key, rootKey+"/") {
		t.Fatalf("resourcePath(%q) = %q, which is not inside %q", ".. ", key, rootKey)
	}
}

// The coordinator must still see a conflict between a tree claim and a write to a
// name inside that tree, including a name that only looks like a parent reference.
func TestAWriteNamedLikeAParentStillConflictsWithItsTree(t *testing.T) {
	workspace := t.TempDir()
	command, err := NewBash(workspace, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	write, err := NewWriteFile(workspace)
	if err != nil {
		t.Fatal(err)
	}
	tree := normalizeConcurrencySpec(command.ClassifyConcurrency(nil, policy.Outcome{Classification: policy.CommandReadOnly}))
	named := normalizeConcurrencySpec(write.ClassifyConcurrency(json.RawMessage(`{"path":".. "}`), policy.Outcome{}))
	if tree.Mode != ConcurrencyClaimed || named.Mode != ConcurrencyClaimed {
		t.Fatalf("tree=%#v named=%#v", tree, named)
	}
	// The claim is never *outside* the workspace it was made in, which is the
	// property the fuzzer found broken; on Windows it lands on the root itself.
	if key := named.Claims[0].Path; key != tree.Claims[0].Path && !strings.HasPrefix(key, tree.Claims[0].Path+"/") {
		t.Fatalf("the claim %q is outside the tree %q", key, tree.Claims[0].Path)
	}
	if !concurrencyConflicts(tree, named) {
		t.Fatalf("a write named %q was not seen as conflicting with the tree %q", named.Claims[0].Path, tree.Claims[0].Path)
	}
}
