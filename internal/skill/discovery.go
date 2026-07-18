package skill

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type ScanRoot struct {
	Path    string `json:"path"`
	Source  Source `json:"source"`
	Exists  bool   `json:"exists"`
	Trusted bool   `json:"trusted"`
}

type DiscoveryOptions struct {
	Workspace string
	Home      string
	Trust     *TrustStore
	Builtins  []Skill
}

func Discover(options DiscoveryOptions) (*Registry, error) {
	workspace, err := normalizeWorkspace(options.Workspace)
	if err != nil {
		return nil, err
	}
	home := options.Home
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	projectTrusted := false
	if options.Trust != nil {
		projectTrusted = options.Trust.IsTrusted(workspace)
	}
	roots := []ScanRoot{
		{Path: filepath.Join(workspace, ".eylu", "skills"), Source: SourceProjectEylu, Trusted: projectTrusted},
		{Path: filepath.Join(workspace, ".agents", "skills"), Source: SourceProjectAgents, Trusted: projectTrusted},
		{Path: filepath.Join(home, ".eylu", "skills"), Source: SourceUserEylu, Trusted: true},
		{Path: filepath.Join(home, ".agents", "skills"), Source: SourceUserAgents, Trusted: true},
	}
	records := make([]Record, 0)
	scannedDirectories := 0
	limitReached := false
	for rootIndex := range roots {
		root := &roots[rootIndex]
		entries, readErr := os.ReadDir(root.Path)
		if os.IsNotExist(readErr) {
			continue
		}
		if readErr != nil {
			records = append(records, Record{Skill: Skill{Root: root.Path, Source: root.Source}, Status: StatusInvalid, Reason: readErr.Error()})
			continue
		}
		root.Exists = true
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
		for _, entry := range entries {
			if !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 || ignoredSkillDirectory(entry.Name()) {
				continue
			}
			if scannedDirectories >= MaxScannedDirectories {
				records = append(records, Record{Skill: Skill{Root: root.Path, Source: root.Source}, Status: StatusInvalid, Reason: fmt.Sprintf("directory scan truncated at %d", MaxScannedDirectories)})
				limitReached = true
				break
			}
			scannedDirectories++
			directory := filepath.Join(root.Path, entry.Name())
			if _, statErr := os.Lstat(filepath.Join(directory, "SKILL.md")); statErr != nil {
				continue
			}
			parsed, parseErr := ParseDirectory(directory, root.Source, root.Trusted)
			if parseErr != nil {
				records = append(records, Record{Skill: Skill{Name: entry.Name(), Entry: filepath.Join(directory, "SKILL.md"), Root: directory, Source: root.Source, Trusted: root.Trusted}, Status: StatusInvalid, Reason: parseErr.Error()})
				continue
			}
			status := StatusActive
			reason := ""
			if root.Source.Project() && !root.Trusted {
				status = StatusUntrusted
				reason = "workspace skills require trust"
			}
			records = append(records, Record{Skill: parsed, Status: status, Reason: reason})
		}
		if limitReached {
			break
		}
	}
	for _, builtin := range options.Builtins {
		builtin.Source = SourceBuiltin
		builtin.Trusted = true
		records = append(records, Record{Skill: builtin, Status: StatusActive})
	}
	applyPrecedence(records)
	enforceActiveLimit(records)
	sort.SliceStable(records, func(i, j int) bool {
		if records[i].Skill.Name != records[j].Skill.Name {
			return records[i].Skill.Name < records[j].Skill.Name
		}
		if records[i].Skill.Source != records[j].Skill.Source {
			return records[i].Skill.Source > records[j].Skill.Source
		}
		return records[i].Skill.Root < records[j].Skill.Root
	})
	return newRegistry(records, roots), nil
}

func ignoredSkillDirectory(name string) bool {
	switch strings.ToLower(name) {
	case ".git", ".hg", ".svn", "node_modules", "vendor":
		return true
	default:
		return false
	}
}

func enforceActiveLimit(records []Record) {
	active := make([]int, 0)
	for index := range records {
		if records[index].Status == StatusActive {
			active = append(active, index)
		}
	}
	sort.Slice(active, func(i, j int) bool {
		left, right := records[active[i]].Skill, records[active[j]].Skill
		if left.Source != right.Source {
			return left.Source > right.Source
		}
		if left.Name != right.Name {
			return left.Name < right.Name
		}
		return left.Root < right.Root
	})
	if len(active) <= MaxActiveSkills {
		return
	}
	for _, index := range active[MaxActiveSkills:] {
		records[index].Status = StatusInvalid
		records[index].Reason = fmt.Sprintf("valid skill limit reached at %d", MaxActiveSkills)
	}
}

func applyPrecedence(records []Record) {
	winners := make(map[string]int)
	for index := range records {
		record := &records[index]
		if record.Status != StatusActive || record.Skill.Name == "" {
			continue
		}
		winnerIndex, exists := winners[record.Skill.Name]
		if !exists {
			winners[record.Skill.Name] = index
			continue
		}
		winner := &records[winnerIndex]
		if record.Skill.Source > winner.Skill.Source {
			winner.Status = StatusShadowed
			winner.ShadowedBy = record.Skill.Entry
			winners[record.Skill.Name] = index
		} else {
			record.Status = StatusShadowed
			record.ShadowedBy = winner.Skill.Entry
		}
	}
}
