package skill

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseDirectoryFrontmatterAndBody(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Join(root, "pdf-processing")
	content := "\ufeff---\r\nname: pdf-processing\r\ndescription: Process PDFs when users mention forms.\r\nlicense: Apache-2.0\r\ncompatibility: Requires pdftotext\r\nmetadata:\r\n  author: example\r\n  version: \"1\"\r\nallowed-tools: Bash(pdftotext:*) Read\r\n---\r\n\r\n# Instructions\r\n\r\nRead references/guide.md.\r\n"
	writeSkillEntry(t, directory, content)
	parsed, err := ParseDirectory(directory, SourceUserEylu, true)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Name != "pdf-processing" || parsed.Description == "" || parsed.License != "Apache-2.0" || parsed.Metadata["version"] != "1" || parsed.AllowedTools != "Bash(pdftotext:*) Read" || !strings.Contains(parsed.Body, "# Instructions") || len(parsed.Digest) != 64 {
		t.Fatalf("skill = %#v", parsed)
	}
	if parsed.Source != SourceUserEylu || !parsed.Trusted || !filepath.IsAbs(parsed.Entry) {
		t.Fatalf("runtime metadata = %#v", parsed)
	}
}

func TestParseDirectoryValidationMatrix(t *testing.T) {
	tests := []struct {
		name      string
		directory string
		content   string
		want      string
	}{
		{name: "empty body valid", directory: "empty-body", content: "---\nname: empty-body\ndescription: Valid description\n---\n", want: ""},
		{name: "bad yaml", directory: "bad-yaml", content: "---\nname: bad-yaml\ndescription: [broken\n---\n", want: "parse YAML"},
		{name: "missing description", directory: "missing-description", content: "---\nname: missing-description\n---\nbody", want: "description"},
		{name: "uppercase name", directory: "upper", content: "---\nname: Upper\ndescription: value\n---\n", want: "name must"},
		{name: "double hyphen", directory: "double--name", content: "---\nname: double--name\ndescription: value\n---\n", want: "name must"},
		{name: "directory mismatch", directory: "actual-name", content: "---\nname: other-name\ndescription: value\n---\n", want: "must match directory"},
		{name: "long compatibility", directory: "long-compat", content: "---\nname: long-compat\ndescription: value\ncompatibility: " + strings.Repeat("x", 501) + "\n---\n", want: "compatibility"},
		{name: "missing frontmatter", directory: "missing-frontmatter", content: "# body", want: "must start"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			directory := filepath.Join(t.TempDir(), test.directory)
			writeSkillEntry(t, directory, test.content)
			parsed, err := ParseDirectory(directory, SourceBuiltin, true)
			if test.want == "" {
				if err != nil || parsed.Body != "" {
					t.Fatalf("skill=%#v err=%v", parsed, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestParseDirectorySizeAndSymlink(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "too-large")
	writeSkillEntry(t, directory, strings.Repeat("x", MaxEntryBytes+1))
	if _, err := ParseDirectory(directory, SourceBuiltin, true); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("error = %v", err)
	}
}

func writeSkillEntry(t *testing.T, directory, content string) {
	t.Helper()
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "SKILL.md"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
