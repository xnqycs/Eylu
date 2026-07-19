package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

const defaultOutput = "THIRD_PARTY_NOTICES.md"

var releaseTargets = []target{
	{goos: "darwin", goarch: "amd64"},
	{goos: "darwin", goarch: "arm64"},
	{goos: "linux", goarch: "amd64"},
	{goos: "linux", goarch: "arm64"},
	{goos: "windows", goarch: "amd64"},
	{goos: "windows", goarch: "arm64"},
}

type target struct {
	goos   string
	goarch string
}

func (t target) String() string {
	return t.goos + "/" + t.goarch
}

type listedPackage struct {
	Standard bool          `json:"Standard"`
	Module   *listedModule `json:"Module"`
}

type listedModule struct {
	Path    string        `json:"Path"`
	Version string        `json:"Version"`
	Dir     string        `json:"Dir"`
	Main    bool          `json:"Main"`
	Replace *listedModule `json:"Replace"`
}

type component struct {
	name    string
	version string
	dir     string
	source  string
	targets []string
	files   []noticeFile
}

type noticeFile struct {
	name string
	text string
}

type goEnvironment struct {
	root    string
	version string
}

func main() {
	var (
		output string
		check  bool
	)
	flag.StringVar(&output, "output", defaultOutput, "generated notice file")
	flag.BoolVar(&check, "check", false, "verify that the generated notice file is current")
	flag.Parse()

	if err := run(output, check); err != nil {
		fmt.Fprintln(os.Stderr, "generate third-party notices:", err)
		os.Exit(1)
	}
}

func run(output string, check bool) error {
	root, err := moduleRoot()
	if err != nil {
		return err
	}
	if !filepath.IsAbs(output) {
		output = filepath.Join(root, output)
	}

	components, err := collectComponents(root)
	if err != nil {
		return err
	}
	generated, err := renderNotices(components)
	if err != nil {
		return err
	}

	if check {
		current, readErr := os.ReadFile(output)
		if readErr != nil {
			return fmt.Errorf("read %s: %w", output, readErr)
		}
		if !bytes.Equal(current, generated) {
			return fmt.Errorf("%s is out of date; run go run ./scripts/generate-third-party-notices", output)
		}
		fmt.Printf("%s is current\n", filepath.Base(output))
		return nil
	}

	if err := os.WriteFile(output, generated, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", output, err)
	}
	fmt.Printf("wrote %s with %d components\n", filepath.Base(output), len(components))
	return nil
}

func moduleRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("get working directory: %w", err)
	}
	for {
		if _, statErr := os.Stat(filepath.Join(dir, "go.mod")); statErr == nil {
			return dir, nil
		} else if !errors.Is(statErr, os.ErrNotExist) {
			return "", fmt.Errorf("inspect go.mod: %w", statErr)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("go.mod not found")
		}
		dir = parent
	}
}

func collectComponents(root string) ([]component, error) {
	byModule := make(map[string]*component)
	for _, target := range releaseTargets {
		packages, err := listDependencies(root, target)
		if err != nil {
			return nil, err
		}
		for _, pkg := range packages {
			if pkg.Standard || pkg.Module == nil || pkg.Module.Main {
				continue
			}
			module := pkg.Module
			dir := module.Dir
			if module.Replace != nil && module.Replace.Dir != "" {
				dir = module.Replace.Dir
			}
			key := module.Path + "@" + module.Version
			entry := byModule[key]
			if entry == nil {
				entry = &component{
					name:    module.Path,
					version: module.Version,
					dir:     dir,
					source:  moduleSource(module.Path, module.Version),
				}
				byModule[key] = entry
			}
			entry.targets = append(entry.targets, target.String())
		}
	}

	goEnv, err := readGoEnvironment(root)
	if err != nil {
		return nil, err
	}
	standardLibrary := component{
		name:    "Go standard library",
		version: goEnv.version,
		dir:     goEnv.root,
		source:  "https://go.dev/",
	}
	for _, target := range releaseTargets {
		standardLibrary.targets = append(standardLibrary.targets, target.String())
	}
	byModule[standardLibrary.name+"@"+standardLibrary.version] = &standardLibrary

	components := make([]component, 0, len(byModule))
	for _, entry := range byModule {
		files, err := readNoticeFiles(entry.dir)
		if err != nil {
			return nil, fmt.Errorf("%s@%s: %w", entry.name, entry.version, err)
		}
		entry.files = files
		entry.targets = normalizeTargets(entry.targets)
		components = append(components, *entry)
	}
	sort.Slice(components, func(i, j int) bool {
		left := strings.ToLower(components[i].name + "@" + components[i].version)
		right := strings.ToLower(components[j].name + "@" + components[j].version)
		return left < right
	})
	return components, nil
}

func listDependencies(root string, target target) ([]listedPackage, error) {
	cmd := exec.Command("go", "list", "-deps", "-json", "./...")
	cmd.Dir = root
	cmd.Env = withEnvironment(os.Environ(), map[string]string{
		"CGO_ENABLED": "0",
		"GOARCH":      target.goarch,
		"GOOS":        target.goos,
	})
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("prepare go list for %s: %w", target, err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start go list for %s: %w", target, err)
	}

	var packages []listedPackage
	decoder := json.NewDecoder(stdout)
	for {
		var pkg listedPackage
		if err := decoder.Decode(&pkg); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			_ = cmd.Wait()
			return nil, fmt.Errorf("decode go list output for %s: %w", target, err)
		}
		packages = append(packages, pkg)
	}
	if err := cmd.Wait(); err != nil {
		return nil, fmt.Errorf("go list for %s: %w: %s", target, err, strings.TrimSpace(stderr.String()))
	}
	return packages, nil
}

func readGoEnvironment(root string) (goEnvironment, error) {
	cmd := exec.Command("go", "env", "-json", "GOROOT", "GOVERSION")
	cmd.Dir = root
	output, err := cmd.Output()
	if err != nil {
		return goEnvironment{}, fmt.Errorf("read Go environment: %w", err)
	}
	var values struct {
		Root    string `json:"GOROOT"`
		Version string `json:"GOVERSION"`
	}
	if err := json.Unmarshal(output, &values); err != nil {
		return goEnvironment{}, fmt.Errorf("decode Go environment: %w", err)
	}
	if values.Root == "" || values.Version == "" {
		return goEnvironment{}, errors.New("go environment did not report GOROOT and GOVERSION")
	}
	return goEnvironment{root: values.Root, version: values.Version}, nil
}

func withEnvironment(current []string, updates map[string]string) []string {
	result := make([]string, 0, len(current)+len(updates))
	for _, item := range current {
		name, _, found := strings.Cut(item, "=")
		if _, replaced := updates[strings.ToUpper(name)]; found && replaced {
			continue
		}
		result = append(result, item)
	}
	for name, value := range updates {
		result = append(result, name+"="+value)
	}
	return result
}

func readNoticeFiles(dir string) ([]noticeFile, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read module directory: %w", err)
	}
	var files []noticeFile
	for _, entry := range entries {
		if entry.IsDir() || !isNoticeFile(entry.Name()) {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		info, err := entry.Info()
		if err != nil {
			return nil, fmt.Errorf("inspect %s: %w", path, err)
		}
		if !info.Mode().IsRegular() || info.Size() > 2<<20 {
			continue
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
		files = append(files, noticeFile{name: entry.Name(), text: strings.TrimRight(string(content), "\r\n")})
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no LICENSE, COPYING, NOTICE, PATENTS, or COPYRIGHT file found in %s", dir)
	}
	sort.Slice(files, func(i, j int) bool {
		return strings.ToLower(files[i].name) < strings.ToLower(files[j].name)
	})
	return files, nil
}

func isNoticeFile(name string) bool {
	lower := strings.ToLower(name)
	extension := filepath.Ext(lower)
	switch extension {
	case "", ".txt", ".md", ".rst":
	default:
		return false
	}
	for _, prefix := range []string{"license", "licence", "copying", "notice", "patents", "copyright"} {
		if lower == prefix || strings.HasPrefix(lower, prefix+"-") || strings.HasPrefix(lower, prefix+".") {
			return true
		}
	}
	return false
}

func renderNotices(components []component) ([]byte, error) {
	var out bytes.Buffer
	out.WriteString("# Third-Party Notices\n\n")
	out.WriteString("Eylu distributions contain the third-party components listed below. ")
	out.WriteString("Each component remains subject to its own license terms.\n\n")
	out.WriteString("This file is generated from the dependency graphs for darwin, linux, and windows ")
	out.WriteString("on amd64 and arm64. Regenerate it with ")
	out.WriteString("`go run ./scripts/generate-third-party-notices`.\n\n")

	for _, component := range components {
		version := component.version
		if version == "" {
			version = "unversioned"
		}
		fmt.Fprintf(&out, "## %s %s\n\n", component.name, version)
		fmt.Fprintf(&out, "- Source: %s\n", component.source)
		fmt.Fprintf(&out, "- Release targets: %s\n", strings.Join(component.targets, ", "))

		var detected []string
		for _, file := range component.files {
			detected = append(detected, classifyLicense(file.text)...)
		}
		detected = uniqueStrings(detected, false)
		if len(detected) == 0 {
			out.WriteString("- Detected terms: see the original files below\n")
		} else {
			fmt.Fprintf(&out, "- Detected terms: %s\n", strings.Join(detected, ", "))
		}

		for _, file := range component.files {
			fmt.Fprintf(&out, "\n### `%s`\n\n```text\n%s\n```\n", file.name, file.text)
		}
		out.WriteByte('\n')
	}
	return append(bytes.TrimRight(out.Bytes(), "\n"), '\n'), nil
}

func classifyLicense(text string) []string {
	lower := strings.ToLower(text)
	var licenses []string
	if strings.Contains(lower, "apache license") && strings.Contains(lower, "version 2.0") {
		licenses = append(licenses, "Apache-2.0")
	}
	if strings.Contains(lower, "permission is hereby granted, free of charge, to any person obtaining a copy") {
		licenses = append(licenses, "MIT")
	}
	if strings.Contains(lower, "redistributions of source code must retain") {
		if strings.Contains(lower, "neither the name") {
			licenses = append(licenses, "BSD-3-Clause")
		} else {
			licenses = append(licenses, "BSD-2-Clause")
		}
	}
	if strings.Contains(lower, "permission to use, copy, modify, and/or distribute this software for any purpose with or without fee") {
		licenses = append(licenses, "ISC")
	}
	if strings.Contains(lower, "mozilla public license version 2.0") {
		licenses = append(licenses, "MPL-2.0")
	}
	if strings.Contains(lower, "creative commons attribution 4.0 international") {
		licenses = append(licenses, "CC-BY-4.0")
	}
	if strings.Contains(lower, "this is free and unencumbered software released into the public domain") {
		licenses = append(licenses, "Unlicense")
	}
	return uniqueStrings(licenses, false)
}

func normalizeTargets(targets []string) []string {
	return uniqueStrings(targets, true)
}

func uniqueStrings(values []string, sorted bool) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	if sorted {
		sort.Strings(result)
	}
	return result
}

func moduleSource(path, version string) string {
	if version == "" {
		return "https://pkg.go.dev/" + path
	}
	return "https://pkg.go.dev/" + path + "@" + version
}
