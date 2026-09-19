package context

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// TestEveryCategoryIsGraded is what makes the grading exhaustive.
//
// The risk this closes is not that a grade is wrong, it is that a new source of
// context is added and nobody decides who authored it: the category would fall
// through to the fail-closed default without anyone noticing that a decision was
// skipped. The categories are read from the package's own source, so a constant
// added anywhere in the package is caught, not only one added to a list somebody
// remembered to update.
func TestEveryCategoryIsGraded(t *testing.T) {
	declared := declaredCategories(t)
	// A parser that finds nothing would make this test pass while proving
	// nothing, so the number it found is asserted as well.
	if len(declared) != 18 {
		t.Fatalf("found %d declared categories, want 18: %v", len(declared), declared)
	}
	graded := GradedCategories()
	for _, category := range declared {
		if _, ok := graded[category]; !ok {
			t.Fatalf("category %q has no trust level: decide who authored it and add it to trustLevels", category)
		}
	}
	for category := range graded {
		found := false
		for _, name := range declared {
			if name == category {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("trustLevels grades %q, which the package no longer declares", category)
		}
	}
}

// TestTrustLevelsFollowTheSource locks the grading itself: changing a level has
// to be a deliberate edit to this table, not a side effect of another change.
func TestTrustLevelsFollowTheSource(t *testing.T) {
	want := map[Category]TrustLevel{
		CategorySystemPrompt:      TrustHost,
		CategoryTaskState:         TrustHost,
		CategoryOutputReserve:     TrustHost,
		CategoryBuiltinToolSchema: TrustHost,
		CategoryUserMessage:       TrustUser,
		CategoryAgentMessage:      TrustDerived,
		CategoryProjectContext:    TrustDerived,
		CategorySummary:           TrustDerived,
		CategoryDriverState:       TrustDerived,
		CategorySkillCatalog:      TrustExternal,
		CategorySkillBody:         TrustExternal,
		CategorySkillResource:     TrustExternal,
		CategoryMCPInstructions:   TrustExternal,
		CategoryMCPToolSchema:     TrustExternal,
		CategoryMCPResource:       TrustExternal,
		CategoryMCPToolResult:     TrustExternal,
		CategoryBuiltinToolResult: TrustExternal,
		CategoryCodeSlice:         TrustExternal,
	}
	for category, level := range want {
		if got := category.Trust(); got != level {
			t.Fatalf("%q is graded %q, want %q", category, got, level)
		}
	}
	if len(want) != len(GradedCategories()) {
		t.Fatalf("this table covers %d categories and the package grades %d", len(want), len(GradedCategories()))
	}
	// The only context that is an instruction in its own right is what the user
	// asked for; everything Eylu reads is data.
	if !CategoryUserMessage.Untrusted() && CategoryUserMessage.Trust() != TrustUser {
		t.Fatal("a user message is neither trusted as the user's own nor graded")
	}
	for _, category := range []Category{CategoryBuiltinToolResult, CategoryCodeSlice, CategoryMCPToolResult, CategorySkillBody} {
		if !category.Untrusted() {
			t.Fatalf("%q is read from outside the conversation but is not graded as external", category)
		}
	}
	// An unknown category fails closed.
	if unknown := Category("invented"); unknown.Trust() != TrustExternal {
		t.Fatalf("an undeclared category is graded %q, want external", unknown.Trust())
	}
}

// declaredCategories reads every `X Category = "y"` constant the package
// declares, from the source rather than from a list that has to be maintained.
func declaredCategories(t *testing.T) []Category {
	t.Helper()
	fset := token.NewFileSet()
	packages, err := parser.ParseDir(fset, ".", func(info os.FileInfo) bool {
		return !strings.HasSuffix(info.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parse the package: %v", err)
	}
	seen := map[Category]bool{}
	for _, parsed := range packages {
		for _, file := range parsed.Files {
			for _, declaration := range file.Decls {
				group, ok := declaration.(*ast.GenDecl)
				if !ok || group.Tok != token.CONST {
					continue
				}
				for _, spec := range group.Specs {
					values, ok := spec.(*ast.ValueSpec)
					if !ok || values.Type == nil {
						continue
					}
					typed, ok := values.Type.(*ast.Ident)
					if !ok || typed.Name != "Category" {
						continue
					}
					for index := range values.Names {
						if index >= len(values.Values) {
							continue
						}
						literal, ok := values.Values[index].(*ast.BasicLit)
						if !ok || literal.Kind != token.STRING {
							continue
						}
						text, err := strconv.Unquote(literal.Value)
						if err != nil {
							t.Fatalf("unquote %s: %v", literal.Value, err)
						}
						seen[Category(text)] = true
					}
				}
			}
		}
	}
	result := make([]Category, 0, len(seen))
	for category := range seen {
		result = append(result, category)
	}
	sort.Slice(result, func(i, j int) bool { return result[i] < result[j] })
	return result
}
