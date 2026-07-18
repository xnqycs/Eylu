package app

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"Eylu/internal/agent"
	"Eylu/internal/config"
	"Eylu/internal/protocol"
	"Eylu/internal/skill"
	"Eylu/internal/tool"
)

func (r *runtime) loadSkillRuntime(cfg config.Config, opts chatOptions, conversation *agent.Conversation) (*skill.Registry, *skill.Session, error) {
	trustStore, err := skill.OpenTrustStore("")
	if err != nil {
		return nil, nil, &protocol.Error{Code: protocol.ErrConfig, Message: "open workspace trust store", Cause: err}
	}
	registry, err := skill.Discover(skill.DiscoveryOptions{Workspace: cfg.Workspace, Trust: trustStore})
	if err != nil {
		return nil, nil, &protocol.Error{Code: protocol.ErrConfig, Message: "discover skills", Cause: err}
	}
	hasUntrusted := false
	for _, record := range registry.Records() {
		if record.Status == skill.StatusUntrusted {
			hasUntrusted = true
			break
		}
	}
	if hasUntrusted {
		trusted := opts.trustSkills
		if !trusted && isTerminal(r.stdin) && !r.trustPrompted[cfg.Workspace] {
			r.trustPrompted[cfg.Workspace] = true
			trusted = r.confirmSkillTrust(cfg.Workspace)
		}
		if trusted {
			if err := trustStore.Trust(cfg.Workspace); err != nil {
				return nil, nil, &protocol.Error{Code: protocol.ErrConfig, Message: "persist workspace skill trust", Cause: err}
			}
			registry, err = skill.Discover(skill.DiscoveryOptions{Workspace: cfg.Workspace, Trust: trustStore})
			if err != nil {
				return nil, nil, err
			}
		}
	}
	initial := map[string]string{}
	if conversation != nil {
		initial = conversation.ActivatedSkillDigests()
	}
	return registry, skill.NewSession(registry, initial), nil
}

func (r *runtime) confirmSkillTrust(workspace string) bool {
	absolute, _ := filepath.Abs(workspace)
	fmt.Fprintf(r.stderr, "Project skills can inject instructions and reference scripts. Trust project skills in %s? [y/N]: ", absolute)
	reader := r.inputReader
	if reader == nil {
		reader = bufio.NewReader(r.stdin)
	}
	answer, _ := reader.ReadString('\n')
	return strings.EqualFold(strings.TrimSpace(answer), "y") || strings.EqualFold(strings.TrimSpace(answer), "yes")
}

func (r *runtime) skillsCommand() *cobra.Command {
	command := &cobra.Command{Use: "skills", Short: "discover, validate, and diagnose Agent Skills"}
	command.AddCommand(r.skillsListCommand(), r.skillsShowCommand(), r.skillsValidateCommand(), r.skillsDiagnoseCommand(), r.skillsTrustCommand(true), r.skillsTrustCommand(false))
	return command
}

func (r *runtime) skillsListCommand() *cobra.Command {
	return &cobra.Command{Use: "list", Args: cobra.NoArgs, RunE: func(*cobra.Command, []string) error {
		_, registry, err := r.discoverForCommand()
		if err != nil {
			return err
		}
		if r.output == "json" {
			return json.NewEncoder(r.stdout).Encode(map[string]any{"skills": skill.SummarizeRecords(registry.Records())})
		}
		for _, record := range registry.Records() {
			fmt.Fprintf(r.stdout, "%s\t%s\t%s\t%s\n", record.Skill.Name, record.Status, record.Skill.Source.String(), record.Reason)
		}
		return nil
	}}
}

func (r *runtime) skillsShowCommand() *cobra.Command {
	return &cobra.Command{Use: "show <name>", Args: cobra.ExactArgs(1), RunE: func(_ *cobra.Command, args []string) error {
		_, registry, err := r.discoverForCommand()
		if err != nil {
			return err
		}
		item, ok := registry.Get(args[0])
		if !ok {
			return &protocol.Error{Code: protocol.ErrConfig, Message: fmt.Sprintf("active skill %q was not found", args[0])}
		}
		if r.output == "json" {
			return json.NewEncoder(r.stdout).Encode(item)
		}
		fmt.Fprintf(r.stdout, "%s\nsource: %s\ndigest: %s\nentry: %s\n\n%s\n", item.Name, item.Source.String(), item.Digest, item.Entry, item.Body)
		return nil
	}}
}

func (r *runtime) skillsValidateCommand() *cobra.Command {
	return &cobra.Command{Use: "validate <dir>", Args: cobra.ExactArgs(1), RunE: func(_ *cobra.Command, args []string) error {
		item, err := skill.ParseDirectory(args[0], skill.SourceBuiltin, true)
		if err != nil {
			return &protocol.Error{Code: protocol.ErrConfig, Message: err.Error(), Cause: err}
		}
		if r.output == "json" {
			return json.NewEncoder(r.stdout).Encode(map[string]any{"valid": true, "skill": item.Summary()})
		}
		fmt.Fprintf(r.stdout, "valid skill %s (%s)\n", item.Name, item.Digest)
		return nil
	}}
}

func (r *runtime) skillsDiagnoseCommand() *cobra.Command {
	return &cobra.Command{Use: "diagnose", Args: cobra.NoArgs, RunE: func(*cobra.Command, []string) error {
		cfg, registry, err := r.discoverForCommand()
		if err != nil {
			return err
		}
		limits := map[string]int{"entry_bytes": skill.MaxEntryBytes, "resource_bytes": skill.MaxResourceBytes, "scanned_directories": skill.MaxScannedDirectories, "active_skills": skill.MaxActiveSkills, "resources_per_skill": skill.MaxResourcesPerSkill, "resource_depth": skill.MaxResourceDepth}
		if r.output == "json" {
			return json.NewEncoder(r.stdout).Encode(map[string]any{"workspace": cfg.Workspace, "roots": registry.Roots(), "records": skill.SummarizeRecords(registry.Records()), "limits": limits})
		}
		fmt.Fprintf(r.stdout, "Workspace: %s\nLimits: entry=%d resource=%d directories=%d active_skills=%d resources/skill=%d depth=%d\n", cfg.Workspace, skill.MaxEntryBytes, skill.MaxResourceBytes, skill.MaxScannedDirectories, skill.MaxActiveSkills, skill.MaxResourcesPerSkill, skill.MaxResourceDepth)
		for _, root := range registry.Roots() {
			fmt.Fprintf(r.stdout, "root %s source=%s exists=%t trusted=%t\n", root.Path, root.Source.String(), root.Exists, root.Trusted)
		}
		for _, record := range registry.Records() {
			fmt.Fprintf(r.stdout, "skill %s status=%s source=%s shadowed_by=%s reason=%s\n", record.Skill.Name, record.Status, record.Skill.Source.String(), record.ShadowedBy, record.Reason)
		}
		return nil
	}}
}

func (r *runtime) skillsTrustCommand(trust bool) *cobra.Command {
	verb := "trust"
	if !trust {
		verb = "revoke"
	}
	return &cobra.Command{Use: verb, Args: cobra.NoArgs, RunE: func(*cobra.Command, []string) error {
		workspace := r.workspace
		if workspace == "" {
			workspace, _ = os.Getwd()
		}
		store, err := skill.OpenTrustStore("")
		if err != nil {
			return err
		}
		if trust {
			err = store.Trust(workspace)
		} else {
			err = store.Revoke(workspace)
		}
		if err != nil {
			return err
		}
		fmt.Fprintf(r.stdout, "Workspace skill trust %s for %s.\n", verb, workspace)
		return nil
	}}
}

func (r *runtime) discoverForCommand() (config.Config, *skill.Registry, error) {
	loaded, _, err := r.loadManager()
	if err != nil {
		return config.Config{}, nil, err
	}
	store, err := skill.OpenTrustStore("")
	if err != nil {
		return config.Config{}, nil, err
	}
	registry, err := skill.Discover(skill.DiscoveryOptions{Workspace: loaded.Config.Workspace, Trust: store})
	return loaded.Config, registry, err
}

func (r *runtime) handleSkillsSlash(conversation *agent.Conversation, cfg config.Config, opts chatOptions) error {
	registry, _, err := r.loadSkillRuntime(cfg, opts, conversation)
	if err != nil {
		return err
	}
	for _, record := range registry.Records() {
		fmt.Fprintf(r.stdout, "%s\t%s\t%s\t%s\n", record.Skill.Name, record.Status, record.Skill.Source.String(), record.Reason)
	}
	return nil
}

func (r *runtime) activateSkillSlash(conversation *agent.Conversation, cfg config.Config, opts chatOptions, name string) error {
	registry, session, err := r.loadSkillRuntime(cfg, opts, conversation)
	if err != nil {
		return err
	}
	if _, ok := registry.Get(name); !ok {
		return &protocol.Error{Code: protocol.ErrConfig, Message: fmt.Sprintf("active skill %q was not found", name)}
	}
	activationTool := tool.NewActivateSkill(registry, session)
	input, _ := json.Marshal(map[string]string{"name": name})
	result := activationTool.Execute(context.Background(), input)
	if result.IsError {
		return &protocol.Error{Code: protocol.ErrTool, Message: result.Content}
	}
	if result.Metadata != nil {
		result.Metadata["trigger"] = "user"
	}
	conversation.RegisterSkillResult(result)
	fmt.Fprintln(r.stdout, result.Content)
	fmt.Fprintf(r.stderr, "[skill] name=%s source=%s digest=%s trigger=user activated_at=%s allowed_tools=%q\n", name, result.Metadata["skill_source"], result.Metadata["skill_digest"], result.Metadata["activated_at"], result.Metadata["allowed_tools"])
	return nil
}
