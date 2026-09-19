package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const sampleOutput = `Eylu		coverage: 0.0% of statements
Eylu/cmd/agent		coverage: 0.0% of statements
ok  	Eylu/internal/agent	2.528s	coverage: 85.2% of statements
ok  	Eylu/internal/buildinfo	1.921s	coverage: 100.0% of statements
ok  	Eylu/internal/driver	10.284s	coverage: 54.5% of statements
?   	Eylu/internal/driver/anthropic_messages	[no test files]
FAIL	Eylu/internal/tool	22.204s	coverage: 81.7% of statements
`

func TestParseMeasurementsReadsEveryPackageAndIgnoresTheRest(t *testing.T) {
	measured, err := parseMeasurements(sampleOutput)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]float64{
		"Eylu":                    0,
		"Eylu/cmd/agent":          0,
		"Eylu/internal/agent":     85.2,
		"Eylu/internal/buildinfo": 100.0,
		"Eylu/internal/driver":    54.5,
		"Eylu/internal/tool":      81.7,
	}
	if len(measured) != len(want) {
		t.Fatalf("measured = %#v, want %d packages", measured, len(want))
	}
	for _, item := range measured {
		expected, present := want[item.pkg]
		if !present {
			t.Fatalf("unexpected package %q", item.pkg)
		}
		if item.percent != expected {
			t.Fatalf("%s = %.1f%%, want %.1f%%", item.pkg, item.percent, expected)
		}
	}
	// The order is stable, so the report does not move between runs.
	for index := 1; index < len(measured); index++ {
		if measured[index-1].pkg > measured[index].pkg {
			t.Fatalf("the packages are not sorted: %#v", measured)
		}
	}
	if _, err := parseMeasurements("ok Eylu/internal/agent coverage: not-a-number% of statements\n"); err == nil {
		t.Fatal("an unreadable percentage was accepted")
	}
}

func floorFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "coverage-floor.txt")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestReadFloorIgnoresCommentsAndRefusesMalformedLines(t *testing.T) {
	path := floorFile(t, "# a comment\n\nEylu/internal/agent 85.2\nEylu/internal/driver 54.5\n")
	floor, exists, err := readFloor(path)
	if err != nil {
		t.Fatal(err)
	}
	if !exists || len(floor) != 2 || floor["Eylu/internal/agent"] != 85.2 {
		t.Fatalf("floor = %#v (exists %t)", floor, exists)
	}
	// A file that is not there is reported as missing rather than as empty, so a
	// comparison can refuse it while a first recording can create it.
	if _, exists, err := readFloor(filepath.Join(t.TempDir(), "absent")); err != nil || exists {
		t.Fatalf("a missing floor file reported exists=%t, %v", exists, err)
	}
	for name, content := range map[string]string{
		"no percentage": "Eylu/internal/agent\n",
		"not a number":  "Eylu/internal/agent most\n",
	} {
		if _, _, err := readFloor(floorFile(t, content)); err == nil {
			t.Fatalf("a malformed floor (%s) was accepted", name)
		}
	}
}

// A package below its floor fails, and the message says which one and by how much.
func TestCompareReportsARegression(t *testing.T) {
	path := floorFile(t, "Eylu/internal/agent 85.2\nEylu/internal/driver 60.0\n")
	measured := []measurement{{pkg: "Eylu/internal/agent", percent: 85.2}, {pkg: "Eylu/internal/driver", percent: 54.5}}
	err := compare(path, map[string]float64{"Eylu/internal/agent": 85.2, "Eylu/internal/driver": 60.0}, measured)
	if err == nil || !strings.Contains(err.Error(), "Eylu/internal/driver 54.5% < 60.0%") {
		t.Fatalf("compare = %v", err)
	}
	if !strings.Contains(err.Error(), "never lowered") {
		t.Fatalf("the failure does not state the rule: %v", err)
	}
}

// A package that is not in the floor is reported rather than enforced, so adding a
// package is a deliberate act rather than a silent one.
func TestCompareReportsAnUnrecordedPackage(t *testing.T) {
	measured := []measurement{{pkg: "Eylu/internal/agent", percent: 85.2}, {pkg: "Eylu/internal/new", percent: 40.0}}
	err := compare("floor", map[string]float64{"Eylu/internal/agent": 85.2}, measured)
	if err == nil || !strings.Contains(err.Error(), "Eylu/internal/new") || !strings.Contains(err.Error(), "no floor") {
		t.Fatalf("compare = %v", err)
	}
}

// Coverage that improved is reported so the floor is raised, and a floor that is
// exactly met passes.
func TestCompareReportsAnImprovementAndAcceptsAnExactMatch(t *testing.T) {
	measured := []measurement{{pkg: "Eylu/internal/agent", percent: 90.0}}
	err := compare("floor", map[string]float64{"Eylu/internal/agent": 85.0}, measured)
	if err == nil || !strings.Contains(err.Error(), "cover more than their floor") {
		t.Fatalf("compare = %v", err)
	}
	if err := compare("floor", map[string]float64{"Eylu/internal/agent": 90.0}, measured); err != nil {
		t.Fatalf("an exact match was refused: %v", err)
	}
	// A fraction below is rounding, not a regression: the summary is printed with
	// one decimal, so a package cannot be asked to reproduce more precision than
	// the report carries.
	if err := compare("floor", map[string]float64{"Eylu/internal/agent": 90.04}, measured); err != nil {
		t.Fatalf("a rounding difference was treated as a regression: %v", err)
	}
}

// The floor file is rewritten from the measurement, and a floor that would drop is
// refused: the rule the tool exists to enforce applies to the tool itself.
func TestUpdateRaisesFloorsAndRefusesToLowerThem(t *testing.T) {
	path := floorFile(t, "Eylu/internal/agent 85.0\n")
	measured := []measurement{{pkg: "Eylu/internal/agent", percent: 90.5}, {pkg: "Eylu/internal/new", percent: 40.0}}
	if err := writeFloor(path, map[string]float64{"Eylu/internal/agent": 85.0}, measured); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), "Eylu/internal/agent 90.5") || !strings.Contains(string(content), "Eylu/internal/new 40.0") {
		t.Fatalf("the floor was not recorded:\n%s", content)
	}
	if !strings.Contains(string(content), "never lowered") {
		t.Fatalf("the recorded floor does not state the rule:\n%s", content)
	}
	// A rewrite of the same measurement is stable.
	if err := writeFloor(path, map[string]float64{"Eylu/internal/agent": 90.5, "Eylu/internal/new": 40.0}, measured); err != nil {
		t.Fatal(err)
	}
	again, _ := os.ReadFile(path)
	if string(again) != string(content) {
		t.Fatal("rewriting an unchanged floor changed the file")
	}

	// A measurement below a committed floor is refused rather than recorded.
	lower := []measurement{{pkg: "Eylu/internal/agent", percent: 80.0}}
	err = writeFloor(path, map[string]float64{"Eylu/internal/agent": 90.5}, lower)
	if err == nil || !strings.Contains(err.Error(), "below the committed floor") {
		t.Fatalf("writeFloor = %v", err)
	}
	after, _ := os.ReadFile(path)
	if string(after) != string(content) {
		t.Fatal("a refused update rewrote the floor")
	}
}
