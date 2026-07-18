package skill

import (
	"encoding/json"
	"time"
)

const (
	MaxEntryBytes         = 512 << 10
	MaxResourceBytes      = 1 << 20
	MaxScannedDirectories = 2000
	MaxActiveSkills       = 200
	MaxResourcesPerSkill  = 2000
	MaxResourceDepth      = 6
)

type Source int

const (
	SourceBuiltin Source = iota
	SourceUserAgents
	SourceUserEylu
	SourceProjectAgents
	SourceProjectEylu
)

func (s Source) String() string {
	switch s {
	case SourceUserAgents:
		return "user_agents"
	case SourceUserEylu:
		return "user_eylu"
	case SourceProjectAgents:
		return "project_agents"
	case SourceProjectEylu:
		return "project_eylu"
	default:
		return "builtin"
	}
}

func (s Source) Project() bool {
	return s == SourceProjectAgents || s == SourceProjectEylu
}

func (s Source) MarshalJSON() ([]byte, error) {
	return json.Marshal(s.String())
}

type Skill struct {
	Name          string            `json:"name" yaml:"name"`
	Description   string            `json:"description" yaml:"description"`
	License       string            `json:"license,omitempty" yaml:"license,omitempty"`
	Compatibility string            `json:"compatibility,omitempty" yaml:"compatibility,omitempty"`
	Metadata      map[string]string `json:"metadata,omitempty" yaml:"metadata,omitempty"`
	AllowedTools  string            `json:"allowed_tools,omitempty" yaml:"allowed-tools,omitempty"`
	Body          string            `json:"body,omitempty" yaml:"-"`
	Entry         string            `json:"entry" yaml:"-"`
	Root          string            `json:"root" yaml:"-"`
	Digest        string            `json:"digest" yaml:"-"`
	Source        Source            `json:"source" yaml:"-"`
	Trusted       bool              `json:"trusted" yaml:"-"`
}

type Status string

const (
	StatusActive    Status = "active"
	StatusShadowed  Status = "shadowed"
	StatusInvalid   Status = "invalid"
	StatusUntrusted Status = "untrusted"
)

type Record struct {
	Skill      Skill  `json:"skill"`
	Status     Status `json:"status"`
	Reason     string `json:"reason,omitempty"`
	ShadowedBy string `json:"shadowed_by,omitempty"`
}

type SkillSummary struct {
	Name          string            `json:"name,omitempty"`
	Description   string            `json:"description,omitempty"`
	License       string            `json:"license,omitempty"`
	Compatibility string            `json:"compatibility,omitempty"`
	Metadata      map[string]string `json:"metadata,omitempty"`
	AllowedTools  string            `json:"allowed_tools,omitempty"`
	Entry         string            `json:"entry,omitempty"`
	Root          string            `json:"root,omitempty"`
	Digest        string            `json:"digest,omitempty"`
	Source        Source            `json:"source"`
	Trusted       bool              `json:"trusted"`
}

type RecordSummary struct {
	Skill      SkillSummary `json:"skill"`
	Status     Status       `json:"status"`
	Reason     string       `json:"reason,omitempty"`
	ShadowedBy string       `json:"shadowed_by,omitempty"`
}

func (s Skill) Summary() SkillSummary {
	return SkillSummary{
		Name: s.Name, Description: s.Description, License: s.License, Compatibility: s.Compatibility,
		Metadata: s.Metadata, AllowedTools: s.AllowedTools, Entry: s.Entry, Root: s.Root, Digest: s.Digest,
		Source: s.Source, Trusted: s.Trusted,
	}
}

func SummarizeRecords(records []Record) []RecordSummary {
	summaries := make([]RecordSummary, 0, len(records))
	for _, record := range records {
		summaries = append(summaries, RecordSummary{
			Skill: record.Skill.Summary(), Status: record.Status, Reason: record.Reason, ShadowedBy: record.ShadowedBy,
		})
	}
	return summaries
}

type Activation struct {
	Name         string    `json:"name"`
	Source       Source    `json:"source"`
	Entry        string    `json:"entry"`
	Root         string    `json:"root"`
	Digest       string    `json:"digest"`
	AllowedTools string    `json:"allowed_tools,omitempty"`
	Resources    []string  `json:"resources,omitempty"`
	Body         string    `json:"body"`
	Trigger      string    `json:"trigger"`
	ActivatedAt  time.Time `json:"activated_at"`
	Duplicate    bool      `json:"duplicate,omitempty"`
}
