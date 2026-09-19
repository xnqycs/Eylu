package docscheck

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const architectureDocument = "docs/architecture-and-reliability.md"

// TestInvariantTableTestsExist is the acceptance test for the document's
// section 8: every test the invariant table names must actually exist. Removing
// or renaming a test without updating the table fails here instead of silently
// leaving the document claiming a guarantee nothing holds up.
func TestInvariantTableTestsExist(t *testing.T) {
	root, err := FindRepositoryRoot(".")
	if err != nil {
		t.Fatalf("locate repository root: %v", err)
	}
	doc, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(architectureDocument)))
	if err != nil {
		t.Fatalf("read %s: %v", architectureDocument, err)
	}
	invariants, err := ParseInvariantTable(string(doc))
	if err != nil {
		t.Fatalf("parse the invariant table: %v", err)
	}

	// A checker that matches nothing is worse than no checker: it reports
	// success while verifying nothing. These bounds are deliberately loose so
	// that adding invariants is free, but a table that stops parsing is not.
	if len(invariants) < 10 {
		t.Fatalf("parsed only %d invariant rows; the table or its heading must have changed shape", len(invariants))
	}
	named := 0
	for _, invariant := range invariants {
		for _, ref := range invariant.Refs {
			if ref.Test != "" {
				named++
			}
		}
	}
	if named < 10 {
		t.Fatalf("parsed only %d named test references; the table format or the reference syntax must have changed", named)
	}

	problems := VerifyInvariants(root, invariants)
	if len(problems) == 0 {
		return
	}
	var report strings.Builder
	for _, problem := range problems {
		report.WriteString("\n  ")
		report.WriteString(problem.Error())
	}
	t.Fatalf("the invariant table names %d test reference(s) that do not hold:%s", len(problems), report.String())
}

func TestParseInvariantTableReadsEveryCellForm(t *testing.T) {
	doc := strings.Join([]string{
		"# A document",
		"",
		"## 8. Invariants and where they are tested",
		"",
		"| invariant | test |",
		"|---|---|",
		"| I-99 a file-only row | `internal/buildinfo/buildinfo_test.go` |",
		"| I-98 several names in one cell | `internal/buildinfo/buildinfo_test.go`: `TestCurrentAndString`, `TestOther` |",
		"| the classifier reads a line as the shell does | `internal/buildinfo/buildinfo_test.go` |",
		"",
		"## 9. Known limitations",
		"",
		"| not a table row of section 8 | `internal/nothing_test.go` |",
	}, "\n")

	invariants, err := ParseInvariantTable(doc)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(invariants) != 3 {
		t.Fatalf("rows = %d, want 3 (%#v)", len(invariants), invariants)
	}
	if invariants[0].Label != "I-99" || invariants[1].Label != "I-98" || invariants[2].Label != "" {
		t.Fatalf("labels = %q, %q, %q", invariants[0].Label, invariants[1].Label, invariants[2].Label)
	}
	if got := invariants[2].Claim; !strings.HasPrefix(got, "the classifier reads") {
		t.Fatalf("unnamed row claim = %q", got)
	}
	want := []Reference{
		{File: "internal/buildinfo/buildinfo_test.go"},
		{File: "internal/buildinfo/buildinfo_test.go", Test: "TestCurrentAndString"},
		{File: "internal/buildinfo/buildinfo_test.go", Test: "TestOther"},
	}
	got := append(append([]Reference{}, invariants[0].Refs...), invariants[1].Refs...)
	if len(got) != len(want) {
		t.Fatalf("references = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("reference %d = %#v, want %#v", i, got[i], want[i])
		}
	}
}

func TestParseInvariantTableRejectsAMissingSection(t *testing.T) {
	if _, err := ParseInvariantTable("# A document\n\n## 7. Something else\n"); err == nil {
		t.Fatal("a document without the invariant section must not parse")
	}
	if _, err := ParseInvariantTable("## 8. Invariants and where they are tested\n\nNo table here.\n"); err == nil {
		t.Fatal("a section without a table must not parse")
	}
}

// TestVerifyInvariantsCatchesAMisspelledTestName is the machine-checkable form
// of the manual acceptance step: break the mapping, and the checker says so.
func TestVerifyInvariantsCatchesAMisspelledTestName(t *testing.T) {
	root, err := FindRepositoryRoot(".")
	if err != nil {
		t.Fatalf("locate repository root: %v", err)
	}
	doc := strings.Join([]string{
		"## 8. Invariants and where they are tested",
		"",
		"| invariant | test |",
		"|---|---|",
		"| I-97 a real test | `internal/buildinfo/buildinfo_test.go`: `TestCurrentAndString` |",
		"| I-96 a misspelled test | `internal/buildinfo/buildinfo_test.go`: `TestCurrentAndStrng` |",
		"| I-95 a missing file | `internal/buildinfo/gone_test.go`: `TestCurrentAndString` |",
		"| I-94 a test name with no file | `TestCurrentAndString` |",
		"| I-93 a row that names nothing | (see above) |",
	}, "\n")
	invariants, err := ParseInvariantTable(doc)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	problems := VerifyInvariants(root, invariants)
	reasons := map[string]string{}
	for _, problem := range problems {
		reasons[problem.Invariant] = problem.Reason
	}
	if len(problems) != 4 {
		t.Fatalf("problems = %#v, want 4", problems)
	}
	if _, ok := reasons["I-97"]; ok {
		t.Fatalf("a reference that holds was reported: %#v", problems)
	}
	for label, want := range map[string]string{
		"I-96": "the named test function is not declared in this file",
		"I-95": "the referenced test file does not exist",
		"I-94": "a test name appears before any test file",
		"I-93": "the row names no test file",
	} {
		if got := reasons[label]; !strings.HasPrefix(got, want) {
			t.Fatalf("%s: reason = %q, want a reason starting with %q", label, got, want)
		}
	}
	found := false
	for _, problem := range problems {
		if problem.Invariant == "I-96" && strings.Contains(problem.Error(), "TestCurrentAndStrng") {
			found = true
		}
	}
	if !found {
		t.Fatalf("the report must name both the invariant and the failing test: %#v", problems)
	}
}

func TestFindRepositoryRootWalksUp(t *testing.T) {
	root, err := FindRepositoryRoot(filepath.Join("internal", "docscheck"))
	if err != nil {
		t.Fatalf("find root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("the reported root has no go.mod: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(architectureDocument))); err != nil {
		t.Fatalf("the reported root does not hold the architecture document: %v", err)
	}
}
