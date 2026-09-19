package docscheck

import (
	"fmt"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// updateGoldenEnv makes a contract test rewrite its golden file instead of
// failing. Regenerating is a deliberate act and shows up as a diff, which is the
// review of a format change.
const updateGoldenEnv = "EYLU_UPDATE_GOLDEN"

// FieldContract is the set of JSON names a program-facing value carries, mapped
// to whether the field is optional.
type FieldContract map[string]bool

// DescribeFields returns the JSON names a value serializes under.
//
// It is what turns "these fields are a promise" from a sentence in a document
// into something a test can compare: the names are read from the same struct tags
// the encoder uses, so the contract cannot describe something the output does not
// do.
func DescribeFields(value any) (FieldContract, error) {
	fields := FieldContract{}
	if err := collectFields(reflect.TypeOf(value), fields); err != nil {
		return nil, err
	}
	return fields, nil
}

func collectFields(typ reflect.Type, fields FieldContract) error {
	for typ != nil && typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	if typ == nil {
		return fmt.Errorf("a nil value has no fields")
	}
	switch typ.Kind() {
	case reflect.Map:
		// A map is its own contract: the keys are the field names it writes.
		if typ.Key().Kind() != reflect.String {
			return fmt.Errorf("a %s map has no field names", typ)
		}
		return nil
	case reflect.Struct:
	default:
		return fmt.Errorf("a %s has no field names", typ)
	}
	for index := 0; index < typ.NumField(); index++ {
		field := typ.Field(index)
		tag := field.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name, options, _ := strings.Cut(tag, ",")
		// An embedded struct without a tag of its own is flattened into the same
		// object, which is what the encoder does with it.
		if field.Anonymous && tag == "" {
			nested := field.Type
			for nested.Kind() == reflect.Pointer {
				nested = nested.Elem()
			}
			if nested.Kind() == reflect.Struct {
				if err := collectFields(nested, fields); err != nil {
					return err
				}
				continue
			}
		}
		if !field.IsExported() {
			continue
		}
		if name == "" {
			name = field.Name
		}
		fields[name] = strings.Contains(options, "omitempty")
	}
	return nil
}

// DescribeMapKeys returns the contract of a value that is serialized as an
// object built at run time rather than as a struct.
func DescribeMapKeys(keys []string) FieldContract {
	fields := make(FieldContract, len(keys))
	for _, key := range keys {
		fields[key] = false
	}
	return fields
}

// RenderFieldContract writes a contract in the form the golden files use: one
// field per line, sorted, with `optional` for a field that is omitted when it is
// empty.
func RenderFieldContract(fields FieldContract) string {
	names := make([]string, 0, len(fields))
	for name := range fields {
		names = append(names, name)
	}
	sort.Strings(names)
	var builder strings.Builder
	builder.WriteString("# Every field below is part of this format's promise.\n")
	builder.WriteString("# A field may be added - record it here in the same change, which is\n")
	builder.WriteString("# what makes the addition a reviewed decision - but never renamed,\n")
	builder.WriteString("# removed, or given a different meaning within this major version.\n")
	builder.WriteString("# Regenerate with " + updateGoldenEnv + "=1 go test ./...\n")
	for _, name := range names {
		if fields[name] {
			builder.WriteString(name + " optional\n")
			continue
		}
		builder.WriteString(name + "\n")
	}
	return builder.String()
}

// ParseFieldContract reads a golden file back into a contract.
func ParseFieldContract(content string) FieldContract {
	fields := FieldContract{}
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, options, _ := strings.Cut(line, " ")
		fields[name] = strings.Contains(options, "optional")
	}
	return fields
}

// CompareFieldContracts reports how a contract changed.
//
// A field that disappeared was removed or renamed, which breaks every reader
// written against it. A field that appeared is an addition, which is allowed, but
// it has to be recorded: the contract is the document a downstream reader trusts,
// and one that silently lags behind the code is worse than none.
func CompareFieldContracts(golden, current FieldContract) []string {
	var problems []string
	for name, wasOptional := range golden {
		optional, present := current[name]
		switch {
		case !present:
			problems = append(problems, fmt.Sprintf("%s was removed or renamed, which breaks every reader of the old shape", name))
		case optional != wasOptional:
			problems = append(problems, fmt.Sprintf("%s changed from optional=%t to optional=%t", name, wasOptional, optional))
		}
	}
	for name, optional := range current {
		if _, present := golden[name]; present {
			continue
		}
		kind := "required"
		if optional {
			kind = "optional"
		}
		problems = append(problems, fmt.Sprintf("%s is new (%s); record it in the contract", name, kind))
	}
	sort.Strings(problems)
	return problems
}

// CheckFieldContract compares one value's field contract against its golden file.
//
// It is the mechanism behind "a field may be added but never removed": a
// program-facing format is only a promise if something fails when the promise is
// broken, and until now nothing did.
func CheckFieldContract(t *testing.T, goldenPath string, value any) {
	t.Helper()
	current, err := DescribeFields(value)
	if err != nil {
		t.Fatalf("describe the contract of %s: %v", goldenPath, err)
	}
	checkRenderedContract(t, goldenPath, current)
}

// CheckMapContract compares a run-time object's keys against its golden file.
func CheckMapContract(t *testing.T, goldenPath string, keys []string) {
	t.Helper()
	checkRenderedContract(t, goldenPath, DescribeMapKeys(keys))
}

func checkRenderedContract(t *testing.T, goldenPath string, current FieldContract) {
	t.Helper()
	rendered := RenderFieldContract(current)
	if os.Getenv(updateGoldenEnv) == "1" {
		if err := os.WriteFile(goldenPath, []byte(rendered), 0o644); err != nil {
			t.Fatalf("update %s: %v", goldenPath, err)
		}
		t.Logf("rewrote %s", goldenPath)
		return
	}
	content, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("%v\n\nCreate it with %s=1 go test ./...", err, updateGoldenEnv)
	}
	problems := CompareFieldContracts(ParseFieldContract(string(content)), current)
	if len(problems) == 0 {
		return
	}
	var report strings.Builder
	for _, problem := range problems {
		report.WriteString("\n  ")
		report.WriteString(problem)
	}
	t.Fatalf("%s no longer matches the committed contract:%s\n\nIf the change is a pure addition, record it with %s=1 go test ./... and review the diff.",
		goldenPath, report.String(), updateGoldenEnv)
}
