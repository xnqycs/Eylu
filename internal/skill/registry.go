package skill

import (
	"fmt"
	"sort"
	"strings"
	"sync"
)

type Registry struct {
	mu      sync.RWMutex
	active  map[string]Skill
	records []Record
	roots   []ScanRoot
}

func newRegistry(records []Record, roots []ScanRoot) *Registry {
	registry := &Registry{active: make(map[string]Skill), records: records, roots: roots}
	for _, record := range records {
		if record.Status == StatusActive {
			registry.active[record.Skill.Name] = record.Skill
		}
	}
	return registry
}

func (r *Registry) Get(name string) (Skill, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	skill, ok := r.active[name]
	return skill, ok
}

func (r *Registry) Active() []Skill {
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := make([]Skill, 0, len(r.active))
	for _, item := range r.active {
		result = append(result, item)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result
}

func (r *Registry) Records() []Record {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]Record(nil), r.records...)
}

func (r *Registry) Roots() []ScanRoot {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]ScanRoot(nil), r.roots...)
}

func (r *Registry) Catalog() string {
	items := r.Active()
	if len(items) == 0 {
		return ""
	}
	var catalog strings.Builder
	catalog.WriteString("<available_skills>\n")
	for _, item := range items {
		fmt.Fprintf(&catalog, "  <skill><name>%s</name><description>%s</description><source>%s</source></skill>\n", escapeCatalog(item.Name), escapeCatalog(item.Description), item.Source.String())
	}
	catalog.WriteString("</available_skills>")
	return catalog.String()
}

func escapeCatalog(value string) string {
	value = strings.Join(strings.Fields(value), " ")
	replacer := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", "\"", "&quot;", "'", "&apos;")
	return replacer.Replace(value)
}
