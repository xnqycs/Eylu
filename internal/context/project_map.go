package context

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	DefaultProjectMapBytes = 32 << 10
	MaxProjectMapFiles     = 5000
	MaxProjectMapDepth     = 6
	projectMapTreeFiles    = 500
	projectMapRecentFiles  = 20
)

type ProjectMap struct {
	Content   string `json:"content"`
	Files     int    `json:"files"`
	Truncated bool   `json:"truncated"`
}

type projectFile struct {
	path    string
	modTime time.Time
}

func BuildProjectMap(workspace string, maxBytes int) (ProjectMap, error) {
	root, err := filepath.Abs(workspace)
	if err != nil {
		return ProjectMap{}, err
	}
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return ProjectMap{}, fmt.Errorf("workspace is not a directory: %s", root)
	}
	files := make([]projectFile, 0)
	truncated := false
	err = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if path == root {
			return nil
		}
		relative, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return nil
		}
		parts := strings.Split(filepath.ToSlash(relative), "/")
		if entry.IsDir() {
			if projectMapIgnoredDirectory(entry.Name()) || len(parts) > MaxProjectMapDepth {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 || len(parts) > MaxProjectMapDepth {
			return nil
		}
		entryInfo, infoErr := entry.Info()
		if infoErr != nil || !entryInfo.Mode().IsRegular() {
			return nil
		}
		files = append(files, projectFile{path: filepath.ToSlash(relative), modTime: entryInfo.ModTime()})
		if len(files) >= MaxProjectMapFiles {
			truncated = true
			return filepath.SkipAll
		}
		return nil
	})
	if err != nil {
		return ProjectMap{}, err
	}
	sort.Slice(files, func(i, j int) bool { return files[i].path < files[j].path })
	content := renderProjectMap(filepath.Base(root), files, truncated)
	if maxBytes <= 0 {
		maxBytes = DefaultProjectMapBytes
	}
	content, byteTruncated := truncateUTF8Bytes(content, maxBytes, "\n[project map truncated]")
	return ProjectMap{Content: content, Files: len(files), Truncated: truncated || byteTruncated}, nil
}

func renderProjectMap(name string, files []projectFile, truncated bool) string {
	languages := make(map[string]int)
	entries := make([]string, 0)
	configs := make([]string, 0)
	for _, file := range files {
		languages[languageForPath(file.path)]++
		if isEntryPath(file.path) {
			entries = append(entries, file.path)
		}
		if isConfigPath(file.path) {
			configs = append(configs, file.path)
		}
	}
	var output strings.Builder
	fmt.Fprintf(&output, "<project_map name=\"%s\" files=\"%d\" truncated=\"%t\">\n", escapeAttribute(name), len(files), truncated)
	output.WriteString("File tree:\n")
	for index, file := range files {
		if index >= projectMapTreeFiles {
			fmt.Fprintf(&output, "- ... %d more files\n", len(files)-index)
			break
		}
		fmt.Fprintf(&output, "- %s\n", file.path)
	}
	output.WriteString("Languages:\n")
	labels := make([]string, 0, len(languages))
	for label := range languages {
		labels = append(labels, label)
	}
	sort.Strings(labels)
	for _, label := range labels {
		fmt.Fprintf(&output, "- %s: %d\n", label, languages[label])
	}
	renderPathList(&output, "Entry points", entries)
	renderPathList(&output, "Configuration", configs)
	recent := append([]projectFile(nil), files...)
	sort.SliceStable(recent, func(i, j int) bool {
		if recent[i].modTime.Equal(recent[j].modTime) {
			return recent[i].path < recent[j].path
		}
		return recent[i].modTime.After(recent[j].modTime)
	})
	output.WriteString("Recently modified:\n")
	for index, file := range recent {
		if index >= projectMapRecentFiles {
			break
		}
		fmt.Fprintf(&output, "- %s\n", file.path)
	}
	output.WriteString("</project_map>")
	return output.String()
}

func renderPathList(output *strings.Builder, heading string, paths []string) {
	output.WriteString(heading + ":\n")
	if len(paths) == 0 {
		output.WriteString("- none\n")
		return
	}
	for _, path := range paths {
		fmt.Fprintf(output, "- %s\n", path)
	}
}

func projectMapIgnoredDirectory(name string) bool {
	switch strings.ToLower(name) {
	case ".git", ".hg", ".svn", ".eylu", "node_modules", "vendor", "dist", "build", "coverage", ".idea", ".vscode":
		return true
	default:
		return false
	}
}

func languageForPath(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".go":
		return "Go"
	case ".ts", ".tsx":
		return "TypeScript"
	case ".js", ".jsx", ".mjs", ".cjs":
		return "JavaScript"
	case ".py":
		return "Python"
	case ".rs":
		return "Rust"
	case ".java":
		return "Java"
	case ".c", ".h", ".cc", ".cpp", ".hpp":
		return "C/C++"
	case ".md":
		return "Markdown"
	case ".json", ".yaml", ".yml", ".toml", ".ini":
		return "Configuration"
	default:
		return "Other"
	}
}

func isEntryPath(path string) bool {
	name := strings.ToLower(filepath.Base(path))
	switch name {
	case "main.go", "main.py", "app.py", "index.js", "index.ts", "main.ts", "main.js", "program.cs":
		return true
	default:
		return strings.HasPrefix(filepath.ToSlash(path), "cmd/") && name == "main.go"
	}
}

func isConfigPath(path string) bool {
	name := strings.ToLower(filepath.Base(path))
	switch name {
	case "go.mod", "go.sum", "package.json", "tsconfig.json", "cargo.toml", "pyproject.toml", "requirements.txt", "dockerfile", "compose.yaml", "compose.yml", "makefile", ".goreleaser.yaml", ".goreleaser.yml":
		return true
	default:
		return strings.HasSuffix(name, ".toml") || strings.HasSuffix(name, ".yaml") || strings.HasSuffix(name, ".yml")
	}
}

func escapeAttribute(value string) string {
	return strings.NewReplacer("&", "&amp;", "\"", "&quot;", "<", "&lt;", ">", "&gt;").Replace(value)
}

func truncateUTF8Bytes(value string, limit int, marker string) (string, bool) {
	if limit <= 0 || len(value) <= limit {
		return value, false
	}
	if len(marker) >= limit {
		marker = marker[:limit]
		for !utf8.ValidString(marker) && len(marker) > 0 {
			marker = marker[:len(marker)-1]
		}
		return marker, true
	}
	end := limit - len(marker)
	for end > 0 && !utf8.RuneStart(value[end]) {
		end--
	}
	return value[:end] + marker, true
}
