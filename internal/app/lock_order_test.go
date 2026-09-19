package app

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"sort"
	"strings"
	"testing"
)

// runtimeLocks are the mutexes the runtime's hierarchy is written about.
var runtimeLocks = []string{"secretMu", "inputMu", "mcpMu", "mcpHostMu", "mcpStateMu", "limitMu", "toolRuntimeMu", "searchTaskMu"}

// TestOnlyTheDocumentedLocksAreNested is what keeps the hierarchy documented on
// `runtime` from drifting away from the code.
//
// A lock order that exists only in the callers is a rule nobody can check, and the
// failure it protects against - two goroutines taking two locks in two orders -
// shows up as a hang nobody can reproduce. The pairs below are read out of the
// package's own source, so adding a nesting site fails here and the author has to
// say where it belongs in the order.
func TestOnlyTheDocumentedLocksAreNested(t *testing.T) {
	pairs := nestedRuntimeLockPairs(t)
	// Both directions are named, because they are not the same mistake: the first
	// is the documented order, the second would be a new one.
	want := []string{"mcpMu>mcpHostMu", "searchTaskMu>toolRuntimeMu"}
	sort.Strings(want)
	if strings.Join(pairs, ",") != strings.Join(want, ",") {
		t.Fatalf("the nested lock pairs are %v, want %v.\n\nIf a new nesting site is deliberate, document it on `runtime` and add it here; if it is not, take the locks one at a time.", pairs, want)
	}
	// A pair that appeared in both directions would be a deadlock waiting for a
	// schedule, and it is worth saying so rather than leaving it to the reader.
	for _, pair := range pairs {
		left, right, _ := strings.Cut(pair, ">")
		for _, other := range pairs {
			if other == right+">"+left {
				t.Fatalf("two locks are taken in both orders: %s and %s", pair, other)
			}
		}
	}
}

// nestedRuntimeLockPairs reads the package's own source and returns every pair of
// runtime locks held at the same time, in the order they were taken.
func nestedRuntimeLockPairs(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read the package directory: %v", err)
	}
	found := map[string]bool{}
	fset := token.NewFileSet()
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, declaration := range file.Decls {
			fn, ok := declaration.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			for _, pair := range nestedPairsInFunction(fset, fn) {
				found[pair] = true
			}
		}
	}
	pairs := make([]string, 0, len(found))
	for pair := range found {
		pairs = append(pairs, pair)
	}
	sort.Strings(pairs)
	return pairs
}

// lockEvent is one acquisition or release of one runtime lock, at one position.
type lockEvent struct {
	pos     token.Pos
	lock    string
	acquire bool
	// deferred marks a release that happens when the function returns, which is
	// what `defer r.xMu.Unlock()` is.
	deferred bool
}

func nestedPairsInFunction(fset *token.FileSet, fn *ast.FuncDecl) []string {
	var events []lockEvent
	ast.Inspect(fn.Body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		lock, action, ok := runtimeLockCall(call)
		if !ok {
			return true
		}
		events = append(events, lockEvent{pos: call.Pos(), lock: lock, acquire: action == "Lock" || action == "RLock"})
		return true
	})
	// A deferred release is the end of the function, not the line it is written on.
	deferred := map[token.Pos]bool{}
	ast.Inspect(fn.Body, func(node ast.Node) bool {
		statement, ok := node.(*ast.DeferStmt)
		if !ok {
			return true
		}
		if lock, action, ok := runtimeLockCall(statement.Call); ok && lock != "" && (action == "Unlock" || action == "RUnlock") {
			deferred[statement.Call.Pos()] = true
		}
		return true
	})
	for index := range events {
		if !events[index].acquire {
			events[index].deferred = deferred[events[index].pos]
		}
	}
	sort.Slice(events, func(i, j int) bool { return events[i].pos < events[j].pos })

	held := make([]string, 0, 2)
	pairs := make([]string, 0, 1)
	for _, event := range events {
		if !event.acquire {
			if event.deferred {
				continue
			}
			for index := range held {
				if held[index] == event.lock {
					held = append(held[:index], held[index+1:]...)
					break
				}
			}
			continue
		}
		for _, outer := range held {
			pairs = append(pairs, outer+">"+event.lock)
		}
		held = append(held, event.lock)
	}
	return pairs
}

// runtimeLockCall reports the runtime lock and the method a call names.
func runtimeLockCall(call *ast.CallExpr) (string, string, bool) {
	selector, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return "", "", false
	}
	inner, ok := selector.X.(*ast.SelectorExpr)
	if !ok {
		return "", "", false
	}
	receiver, ok := inner.X.(*ast.Ident)
	if !ok || receiver.Name != "r" {
		return "", "", false
	}
	for _, lock := range runtimeLocks {
		if inner.Sel.Name == lock {
			return lock, selector.Sel.Name, true
		}
	}
	return "", "", false
}
