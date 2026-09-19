// Package docscheck keeps the documentation's checkable claims bound to the
// code they name.
//
// `docs/architecture-and-reliability.md` section 8 states the invariants and
// names the tests that are supposed to hold them up. That mapping used to be
// prose only: renaming or deleting a test left the table claiming a guarantee
// that no longer had a test behind it, and `go test ./...` stayed green. This
// package reads the table and proves every reference exists, so the document
// fails the build instead of drifting.
package docscheck

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Invariant is one row of the invariant table.
type Invariant struct {
	// Label is the row's leading identifier such as "I-01" or "T-03". It is
	// empty for a row this repository deliberately leaves unnamed.
	Label string
	// Claim is the row's full first cell, kept whole so an error can quote the
	// guarantee rather than only its number.
	Claim string
	// Refs are the files and test functions the row names, in the order the row
	// names them.
	Refs []Reference
}

// Reference names one test file and, when the row is specific, one test
// function inside it.
type Reference struct {
	// File is a repository-relative, slash-separated path ending in `_test.go`.
	File string
	// Test is the bare test function name, or empty when the row points at a
	// whole file.
	Test string
}

// Problem is one reference that does not hold.
type Problem struct {
	// Invariant is the row's label when it has one, otherwise its claim.
	Invariant string
	// Reference is the reference that failed.
	Reference Reference
	// Reason says what is wrong with it.
	Reason string
}

func (p Problem) Error() string {
	ref := p.Reference.File
	if p.Reference.Test != "" {
		ref += ": " + p.Reference.Test
	}
	return fmt.Sprintf("%s: %s: %s", p.Invariant, ref, p.Reason)
}

var (
	backtickPattern = regexp.MustCompile("`([^`]+)`")
	// A fuzz target is a test: the table promises coverage, and a fuzz target is
	// coverage for the inputs no example can enumerate.
	testNamePattern = regexp.MustCompile(`^(Test|Fuzz)[A-Za-z0-9_]*$`)
	labelPattern    = regexp.MustCompile(`^[A-Z]-[0-9]+$`)
)

// ParseInvariantTable reads the invariant table out of the document.
//
// The table is located by its section heading and ends with the first line that
// is not a table row. Within a row, a backticked token ending in `_test.go`
// opens a file, and every following backticked `Test...` token belongs to that
// file until another file opens - which is what makes a cell holding several
// comma-separated test names, or two files in sequence, parse correctly.
func ParseInvariantTable(doc string) ([]Invariant, error) {
	lines := strings.Split(doc, "\n")
	start := -1
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "## 8.") {
			start = i
			break
		}
	}
	if start < 0 {
		return nil, fmt.Errorf("the invariant section heading (\"## 8.\") is missing from the document")
	}

	var (
		invariants    []Invariant
		current       *Invariant
		seenSeparator bool
	)
	flush := func() {
		if current != nil {
			invariants = append(invariants, *current)
			current = nil
		}
	}
	for _, line := range lines[start+1:] {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			break
		}
		if !strings.HasPrefix(trimmed, "|") {
			if seenSeparator {
				break
			}
			continue
		}
		cells := strings.Split(trimmed, "|")
		if len(cells) < 4 {
			return nil, fmt.Errorf("the row %q does not have a claim and a test cell", trimmed)
		}
		claim := strings.TrimSpace(cells[1])
		tests := strings.TrimSpace(cells[2])
		if !seenSeparator {
			// The header row carries no data; the separator row opens the body.
			if isTableSeparator(claim) {
				seenSeparator = true
			}
			continue
		}
		if claim == "" || isTableSeparator(claim) {
			continue
		}
		flush()
		current = &Invariant{Label: labelOf(claim), Claim: claim, Refs: parseReferences(tests)}
	}
	flush()
	if len(invariants) == 0 {
		return nil, fmt.Errorf("the invariant table has no rows")
	}
	return invariants, nil
}

func isTableSeparator(cell string) bool {
	return strings.Trim(cell, "-: ") == ""
}

func labelOf(claim string) string {
	fields := strings.Fields(claim)
	if len(fields) == 0 {
		return ""
	}
	if labelPattern.MatchString(fields[0]) {
		return fields[0]
	}
	return ""
}

func parseReferences(cell string) []Reference {
	var (
		refs  []Reference
		file  string
		names = map[string]bool{}
	)
	for _, match := range backtickPattern.FindAllStringSubmatch(cell, -1) {
		token := strings.TrimSpace(match[1])
		switch {
		case strings.HasSuffix(token, "_test.go"):
			file = token
			refs = append(refs, Reference{File: token})
		case testNamePattern.MatchString(token):
			if file == "" {
				// Report it against the row rather than silently dropping it.
				refs = append(refs, Reference{Test: token})
				continue
			}
			key := file + "\x00" + token
			if names[key] {
				continue
			}
			names[key] = true
			refs = append(refs, Reference{File: file, Test: token})
		}
	}
	// A bare file reference is only interesting when the row names no test
	// inside that file; otherwise it would report the same missing file twice.
	named := map[string]bool{}
	for _, ref := range refs {
		if ref.Test != "" {
			named[ref.File] = true
		}
	}
	kept := refs[:0]
	for _, ref := range refs {
		if ref.Test == "" && ref.File != "" && named[ref.File] {
			continue
		}
		kept = append(kept, ref)
	}
	return kept
}

// VerifyInvariants checks every reference against the repository rooted at
// root and returns one problem per reference that does not hold.
func VerifyInvariants(root string, invariants []Invariant) []Problem {
	var problems []Problem
	for _, invariant := range invariants {
		name := invariant.Label
		if name == "" {
			name = invariant.Claim
		}
		if len(invariant.Refs) == 0 {
			problems = append(problems, Problem{
				Invariant: name,
				Reference: Reference{File: "(row)"},
				Reason:    "the row names no test file, so nothing holds this invariant up",
			})
			continue
		}
		for _, ref := range invariant.Refs {
			if ref.File == "" {
				problems = append(problems, Problem{
					Invariant: name,
					Reference: ref,
					Reason:    "a test name appears before any test file, so it cannot be located",
				})
				continue
			}
			path := filepath.Join(root, filepath.FromSlash(ref.File))
			if _, err := os.Stat(path); err != nil {
				problems = append(problems, Problem{
					Invariant: name,
					Reference: ref,
					Reason:    "the referenced test file does not exist",
				})
				continue
			}
			if ref.Test == "" {
				continue
			}
			found, err := declaresTest(path, ref.Test)
			if err != nil {
				problems = append(problems, Problem{
					Invariant: name,
					Reference: ref,
					Reason:    fmt.Sprintf("the referenced test file cannot be parsed: %v", err),
				})
				continue
			}
			if !found {
				problems = append(problems, Problem{
					Invariant: name,
					Reference: ref,
					Reason:    "the named test function is not declared in this file",
				})
			}
		}
	}
	return problems
}

// declaresTest reports whether the file declares the named test or fuzz target at
// the top level, with the signature the testing package requires.
func declaresTest(path, name string) (bool, error) {
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		return false, err
	}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv != nil || fn.Name == nil || fn.Name.Name != name {
			continue
		}
		return isGoTestSignature(fn, name), nil
	}
	return false, nil
}

// isGoTestSignature reports whether a declaration is a test function or a fuzz
// target. A fuzz target takes *testing.F rather than *testing.T, and a row that
// names one is promising a search rather than an example.
func isGoTestSignature(fn *ast.FuncDecl, name string) bool {
	if fn.Type == nil || fn.Type.Params == nil || len(fn.Type.Params.List) != 1 {
		return false
	}
	// `a, b *testing.T` is one parameter field holding two names, and a test takes
	// exactly one parameter.
	if len(fn.Type.Params.List[0].Names) != 1 {
		return false
	}
	star, ok := fn.Type.Params.List[0].Type.(*ast.StarExpr)
	if !ok {
		return false
	}
	selector, ok := star.X.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := selector.X.(*ast.Ident)
	if !ok || pkg.Name != "testing" || selector.Sel == nil {
		return false
	}
	if strings.HasPrefix(name, "Fuzz") {
		return selector.Sel.Name == "F"
	}
	return selector.Sel.Name == "T"
}

// FindRepositoryRoot walks up from dir until it finds the go.mod that roots the
// checkout, so the test does not depend on where `go test` happens to run.
func FindRepositoryRoot(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(abs, "go.mod")); err == nil {
			return abs, nil
		}
		parent := filepath.Dir(abs)
		if parent == abs {
			return "", fmt.Errorf("no go.mod found above %s", dir)
		}
		abs = parent
	}
}
