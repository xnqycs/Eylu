// Command check-coverage fails when a package's statement coverage falls below the
// floor committed for it, and raises floors when coverage improves.
//
// The rule this exists to enforce is "do not go backwards". It is not "reach a
// number": the plan that introduced it says explicitly that it is not about a
// uniform target, and the four dialect packages that are thirteen lines of
// wrapping are deliberately left out because they have no logic of their own to
// cover.
//
// One property is enforced by the tool itself rather than by a comment. -update
// may raise a floor and may add a package, and it refuses to lower one: a floor
// that could be lowered to make a build pass would be lowered the first time it
// was inconvenient, and the mechanism would quietly stop meaning anything.
// Lowering a floor is therefore an explicit edit to the committed file, which is
// exactly the review this is meant to force.
//
//	go run ./scripts/check-coverage            # compare against the committed floor
//	go run ./scripts/check-coverage -update    # raise floors that improved
package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

const defaultFloor = "scripts/coverage-floor.txt"

type measurement struct {
	pkg     string
	percent float64
}

func main() {
	floorPath := flag.String("floor", defaultFloor, "path of the committed coverage floor")
	patterns := flag.String("packages", "./...", "package patterns passed to go test")
	update := flag.Bool("update", false, "raise floors that improved, and add packages that are new")
	flag.Parse()

	if err := run(*floorPath, *patterns, *update); err != nil {
		fmt.Fprintf(os.Stderr, "check-coverage: %v\n", err)
		os.Exit(1)
	}
}

func run(floorPath, patterns string, update bool) error {
	measured, err := measure(patterns)
	if err != nil {
		return err
	}
	if len(measured) == 0 {
		return fmt.Errorf("no package reported coverage; the pattern %q matched nothing", patterns)
	}
	floor, exists, err := readFloor(floorPath)
	if err != nil {
		return err
	}
	if !exists && !update {
		return fmt.Errorf("no coverage floor is committed at %s; record the current one with: go run ./scripts/check-coverage -update", floorPath)
	}
	if update {
		return writeFloor(floorPath, floor, measured)
	}
	return compare(floorPath, floor, measured)
}

// measure runs the suite with coverage and reads the per-package summary.
//
// The summary is what is parsed rather than a cover profile: a profile would have
// to be joined across packages and would report statements the release build does
// not contain, and the summary is already the number a reader sees.
func measure(patterns string) ([]measurement, error) {
	command := exec.Command("go", "test", "-cover", patterns)
	command.Stderr = os.Stderr
	output, err := command.Output()
	if err != nil {
		return nil, fmt.Errorf("go test -cover failed: %w", err)
	}
	return parseMeasurements(string(output))
}

func parseMeasurements(output string) ([]measurement, error) {
	measured := make([]measurement, 0, 32)
	for _, line := range strings.Split(output, "\n") {
		item, present, err := parseMeasurementLine(line)
		if err != nil {
			return nil, err
		}
		if present {
			measured = append(measured, item)
		}
	}
	sort.Slice(measured, func(i, j int) bool { return measured[i].pkg < measured[j].pkg })
	return measured, nil
}

// parseMeasurementLine reads one summary line.
//
// Any line that mentions coverage has to be read: a line this tool could not parse
// would be a package that silently stopped being measured, which is the failure
// the floor exists to prevent.
func parseMeasurementLine(line string) (measurement, bool, error) {
	if !strings.Contains(line, "coverage:") {
		return measurement{}, false, nil
	}
	_, rest, found := strings.Cut(line, "coverage: ")
	if !found {
		return measurement{}, false, fmt.Errorf("cannot read the coverage line %q", strings.TrimSpace(line))
	}
	percentText, _, found := strings.Cut(rest, "% of statements")
	if !found {
		return measurement{}, false, fmt.Errorf("cannot read the coverage line %q", strings.TrimSpace(line))
	}
	percent, err := strconv.ParseFloat(strings.TrimSpace(percentText), 64)
	if err != nil {
		return measurement{}, false, fmt.Errorf("cannot read the coverage line %q: %w", strings.TrimSpace(line), err)
	}
	fields := strings.Fields(line)
	name := ""
	switch {
	case len(fields) > 0 && (fields[0] == "ok" || fields[0] == "FAIL"):
		if len(fields) < 2 {
			return measurement{}, false, fmt.Errorf("cannot read the package name from %q", strings.TrimSpace(line))
		}
		name = fields[1]
	case len(fields) > 0:
		// The root package's summary carries no `ok` prefix.
		name = fields[0]
	default:
		return measurement{}, false, fmt.Errorf("cannot read the package name from %q", line)
	}
	if !strings.HasPrefix(name, "Eylu") {
		return measurement{}, false, fmt.Errorf("cannot read the package name from %q", strings.TrimSpace(line))
	}
	return measurement{pkg: name, percent: percent}, true, nil
}

// readFloor reads the committed floor, and reports whether one was there at all.
// A package that is not mentioned has no floor: a new package is measured and
// reported, never silently enforced.
func readFloor(path string) (map[string]float64, bool, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]float64{}, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("read the coverage floor %s: %w", path, err)
	}
	defer file.Close()
	floor := map[string]float64{}
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, value, found := strings.Cut(line, " ")
		if !found {
			return nil, true, fmt.Errorf("%s: %q is not a package and a percentage", path, line)
		}
		percent, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
		if err != nil {
			return nil, true, fmt.Errorf("%s: %q has no readable percentage: %w", path, line, err)
		}
		floor[name] = percent
	}
	if err := scanner.Err(); err != nil {
		return nil, true, err
	}
	return floor, true, nil
}

// compare reports every package that fell below its floor, and every package whose
// coverage improved enough to record.
func compare(path string, floor map[string]float64, measured []measurement) error {
	var regressions, improvements, unrecorded []string
	for _, item := range measured {
		committed, recorded := floor[item.pkg]
		if !recorded {
			unrecorded = append(unrecorded, fmt.Sprintf("  %s %.1f%% (no floor yet)", item.pkg, item.percent))
			continue
		}
		if item.percent+0.05 < committed {
			regressions = append(regressions, fmt.Sprintf("  %s %.1f%% < %.1f%%", item.pkg, item.percent, committed))
			continue
		}
		if item.percent > committed+0.05 {
			improvements = append(improvements, fmt.Sprintf("  %s %.1f%% > %.1f%%", item.pkg, item.percent, committed))
		}
	}
	if len(regressions) > 0 {
		return fmt.Errorf("coverage fell below the floor committed in %s:\n%s\n\nA floor is raised by adding tests, never lowered to make a build pass", path, strings.Join(regressions, "\n"))
	}
	if len(unrecorded) > 0 {
		return fmt.Errorf("these packages have no floor in %s:\n%s\n\nRecord them with: go run ./scripts/check-coverage -update", path, strings.Join(unrecorded, "\n"))
	}
	if len(improvements) > 0 {
		return fmt.Errorf("these packages now cover more than their floor records in %s:\n%s\n\nRaise the floor with: go run ./scripts/check-coverage -update", path, strings.Join(improvements, "\n"))
	}
	return nil
}

// writeFloor records the measured coverage, and refuses to lower anything.
func writeFloor(path string, floor map[string]float64, measured []measurement) error {
	for _, item := range measured {
		if committed, recorded := floor[item.pkg]; recorded && item.percent+0.05 < committed {
			return fmt.Errorf("%s: %.1f%% is below the committed floor of %.1f%%; a floor is raised by adding tests, never lowered to make a build pass", item.pkg, item.percent, committed)
		}
	}
	next := map[string]float64{}
	for _, item := range measured {
		next[item.pkg] = item.percent
	}
	names := make([]string, 0, len(next))
	for name := range next {
		names = append(names, name)
	}
	sort.Strings(names)
	var builder strings.Builder
	builder.WriteString("# Statement coverage floors, one package per line.\n")
	builder.WriteString("#\n")
	builder.WriteString("# The rule is \"do not go backwards\", not \"reach a number\". A floor is\n")
	builder.WriteString("# raised by adding tests; it is never lowered to make a build pass, and\n")
	builder.WriteString("# `go run ./scripts/check-coverage -update` refuses to lower one. Lowering\n")
	builder.WriteString("# a floor is an explicit edit to this file, which is the review it forces.\n")
	builder.WriteString("#\n")
	builder.WriteString("# Every package that reports coverage is listed, including the ones that\n")
	builder.WriteString("# report none. The module root, the command entry points and the four dialect\n")
	builder.WriteString("# packages exist to be called rather than to be tested - each dialect package is\n")
	builder.WriteString("# thirteen lines of wrapping around webnative - so a floor of 0.0 for them says\n")
	builder.WriteString("# that rather than pretending they are measured against a target. They are still\n")
	builder.WriteString("# compared: a package that reported coverage before and reports none now falls\n")
	builder.WriteString("# below the floor recorded for it and fails the gate.\n")
	for _, name := range names {
		builder.WriteString(fmt.Sprintf("%s %.1f\n", name, next[name]))
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(builder.String()), 0o644)
}
