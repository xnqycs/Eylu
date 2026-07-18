package skill

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

var validName = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

type frontmatter struct {
	Name          string            `yaml:"name"`
	Description   string            `yaml:"description"`
	License       string            `yaml:"license"`
	Compatibility string            `yaml:"compatibility"`
	Metadata      map[string]string `yaml:"metadata"`
	AllowedTools  string            `yaml:"allowed-tools"`
}

func ParseDirectory(directory string, source Source, trusted bool) (Skill, error) {
	info, err := os.Lstat(directory)
	if err != nil {
		return Skill{}, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return Skill{}, fmt.Errorf("skill root must be a real directory")
	}
	entry := filepath.Join(directory, "SKILL.md")
	entryInfo, err := os.Lstat(entry)
	if err != nil {
		return Skill{}, err
	}
	if !entryInfo.Mode().IsRegular() || entryInfo.Mode()&os.ModeSymlink != 0 {
		return Skill{}, fmt.Errorf("SKILL.md must be a regular file")
	}
	if entryInfo.Size() > MaxEntryBytes {
		return Skill{}, fmt.Errorf("SKILL.md exceeds %d bytes", MaxEntryBytes)
	}
	data, err := os.ReadFile(entry)
	if err != nil {
		return Skill{}, err
	}
	metadata, body, err := parseContent(data)
	if err != nil {
		return Skill{}, err
	}
	if err := validateMetadata(metadata, filepath.Base(directory)); err != nil {
		return Skill{}, err
	}
	root, err := filepath.Abs(directory)
	if err != nil {
		return Skill{}, err
	}
	digest := sha256.Sum256(data)
	return Skill{
		Name: metadata.Name, Description: metadata.Description, License: metadata.License,
		Compatibility: metadata.Compatibility, Metadata: metadata.Metadata, AllowedTools: metadata.AllowedTools,
		Body: body, Entry: filepath.Join(root, "SKILL.md"), Root: root, Digest: hex.EncodeToString(digest[:]), Source: source, Trusted: trusted,
	}, nil
}

func parseContent(data []byte) (frontmatter, string, error) {
	data = bytes.TrimPrefix(data, []byte{0xef, 0xbb, 0xbf})
	normalized := strings.ReplaceAll(string(data), "\r\n", "\n")
	lines := strings.Split(normalized, "\n")
	if len(lines) < 3 || lines[0] != "---" {
		return frontmatter{}, "", fmt.Errorf("SKILL.md must start with YAML frontmatter")
	}
	closing := -1
	for index := 1; index < len(lines); index++ {
		if lines[index] == "---" {
			closing = index
			break
		}
	}
	if closing < 0 {
		return frontmatter{}, "", fmt.Errorf("SKILL.md frontmatter is not closed")
	}
	var metadata frontmatter
	if err := yaml.Unmarshal([]byte(strings.Join(lines[1:closing], "\n")), &metadata); err != nil {
		return frontmatter{}, "", fmt.Errorf("parse YAML frontmatter: %w", err)
	}
	body := strings.TrimSpace(strings.Join(lines[closing+1:], "\n"))
	return metadata, body, nil
}

func validateMetadata(metadata frontmatter, directoryName string) error {
	if len(metadata.Name) < 1 || len(metadata.Name) > 64 || !validName.MatchString(metadata.Name) {
		return fmt.Errorf("name must be 1-64 lowercase letters, numbers, or single hyphens")
	}
	if metadata.Name != directoryName {
		return fmt.Errorf("name %q must match directory %q", metadata.Name, directoryName)
	}
	if len(metadata.Description) < 1 || len(metadata.Description) > 1024 {
		return fmt.Errorf("description must be 1-1024 characters")
	}
	if len(metadata.Compatibility) > 500 {
		return fmt.Errorf("compatibility must be at most 500 characters")
	}
	return nil
}
