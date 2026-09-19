package docscheck

import (
	"strings"
	"testing"
)

func TestCompareFieldContractsReportsOnlyBreakingChanges(t *testing.T) {
	golden := FieldContract{"kept": false, "optional": true}
	current := FieldContract{"kept": false, "optional": true, "added": true}
	problems := CompareFieldContracts(golden, current)
	if len(problems) != 1 || !strings.Contains(problems[0], "added is new") {
		t.Fatalf("problems = %#v", problems)
	}

	for _, testCase := range []struct {
		name    string
		current FieldContract
		want    string
	}{
		{name: "a removal", current: FieldContract{"optional": true}, want: "kept was removed or renamed"},
		{name: "a rename", current: FieldContract{"renamed": false, "optional": true}, want: "kept was removed or renamed"},
		{name: "a field that stopped being optional", current: FieldContract{"kept": false, "optional": false}, want: "optional changed from optional=true to optional=false"},
		{name: "a field that became optional", current: FieldContract{"kept": true, "optional": true}, want: "kept changed from optional=false to optional=true"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			problems := CompareFieldContracts(golden, testCase.current)
			found := false
			for _, problem := range problems {
				if strings.Contains(problem, testCase.want) {
					found = true
				}
			}
			if !found {
				t.Fatalf("problems = %#v, want one naming %q", problems, testCase.want)
			}
		})
	}
	// An unchanged contract reports nothing.
	if problems := CompareFieldContracts(golden, golden); len(problems) != 0 {
		t.Fatalf("an unchanged contract reported %#v", problems)
	}
}

func TestDescribeFieldsReadsTheSameNamesTheEncoderWrites(t *testing.T) {
	type nested struct {
		Inner string `json:"inner"`
	}
	type subject struct {
		Required string `json:"required"`
		Optional string `json:"optional,omitempty"`
		Hidden   string `json:"-"`
		nested
		Untagged string
	}
	fields, err := DescribeFields(subject{})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"required": false, "optional": true, "inner": false, "Untagged": false}
	if len(fields) != len(want) {
		t.Fatalf("fields = %#v, want %#v", fields, want)
	}
	for name, optional := range want {
		got, present := fields[name]
		if !present || got != optional {
			t.Fatalf("field %q = %t (present %t), want %t", name, got, present, optional)
		}
	}

	pointer, err := DescribeFields(&subject{})
	if err != nil || len(pointer) != len(want) {
		t.Fatalf("a pointer described %#v (%v)", pointer, err)
	}
	for _, value := range []any{nil, 7, "text"} {
		if _, err := DescribeFields(value); err == nil {
			t.Fatalf("%#v described a field contract", value)
		}
	}
	// A value with no fields at all describes an empty contract rather than an
	// error; it simply cannot satisfy any golden file.
	if empty, err := DescribeFields(struct{}{}); err != nil || len(empty) != 0 {
		t.Fatalf("an empty struct described %#v (%v)", empty, err)
	}
}

func TestRenderAndParseRoundTrip(t *testing.T) {
	fields := FieldContract{"alpha": false, "beta": true}
	rendered := RenderFieldContract(fields)
	if !strings.Contains(rendered, "alpha\n") || !strings.Contains(rendered, "beta optional\n") {
		t.Fatalf("rendered = %q", rendered)
	}
	parsed := ParseFieldContract(rendered)
	if len(parsed) != 2 || parsed["beta"] != true || parsed["alpha"] != false {
		t.Fatalf("parsed = %#v", parsed)
	}
	if problems := CompareFieldContracts(fields, parsed); len(problems) != 0 {
		t.Fatalf("a round trip changed the contract: %#v", problems)
	}
}
