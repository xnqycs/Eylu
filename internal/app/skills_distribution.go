package app

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"Eylu/internal/config"
	"Eylu/internal/protocol"
	"Eylu/internal/skilldist"
)

func (r *runtime) skillRegistriesCommand() *cobra.Command {
	command := &cobra.Command{Use: "registries", Short: "manage signed Skill registries"}
	command.AddCommand(&cobra.Command{Use: "list", Short: "list configured Skill registries", Args: cobra.NoArgs, RunE: func(*cobra.Command, []string) error {
		loaded, _, err := r.loadManager()
		if err != nil {
			return err
		}
		if r.output != "text" {
			return json.NewEncoder(r.stdout).Encode(map[string]any{"registries": loaded.Config.SkillRegistries})
		}
		names := make([]string, 0, len(loaded.Config.SkillRegistries))
		for name := range loaded.Config.SkillRegistries {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			registry := loaded.Config.SkillRegistries[name]
			fmt.Fprintf(r.stdout, "%s\t%s\tkeys=%d\tdisabled=%t\n", name, registry.IndexURL, len(registry.PublicKeys), registry.Disabled)
		}
		return nil
	}})
	var indexURL, tokenEnvironment string
	var timeout time.Duration
	var publicKeys map[string]string
	add := &cobra.Command{Use: "add <name>", Short: "add or replace a signed Skill registry", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		loaded, _, err := r.loadManager()
		if err != nil {
			return err
		}
		candidate := config.SkillRegistryConfig{IndexURL: indexURL, PublicKeys: publicKeys, TokenEnvironment: tokenEnvironment}
		if cmd.Flags().Changed("timeout") {
			candidate.TimeoutSeconds = int(timeout / time.Second)
		}
		validation := loaded.Config.Clone()
		validation.SkillRegistries[args[0]] = candidate
		if err := validation.Validate(); err != nil {
			return &protocol.Error{Code: protocol.ErrConfig, Message: err.Error()}
		}
		if _, err := loaded.Store.SetSkillRegistry(args[0], candidate); err != nil {
			return &protocol.Error{Code: protocol.ErrConfig, Message: "save Skill registry: " + err.Error(), Cause: err}
		}
		fmt.Fprintf(r.stdout, "Skill registry %s saved.\n", args[0])
		return nil
	}}
	add.Flags().StringVar(&indexURL, "index-url", "", "registry index URL")
	add.Flags().StringToStringVar(&publicKeys, "public-key", nil, "trusted Ed25519 key as id=base64")
	add.Flags().StringVar(&tokenEnvironment, "token-env", "", "environment variable containing an optional bearer token")
	add.Flags().DurationVar(&timeout, "timeout", 30*time.Second, "registry request timeout")
	command.AddCommand(add)
	command.AddCommand(&cobra.Command{Use: "delete <name>", Short: "delete a Skill registry configuration", Args: cobra.ExactArgs(1), RunE: func(_ *cobra.Command, args []string) error {
		loaded, _, err := r.loadManager()
		if err != nil {
			return err
		}
		if _, exists := loaded.Config.SkillRegistries[args[0]]; !exists {
			return &protocol.Error{Code: protocol.ErrConfig, Message: fmt.Sprintf("Skill registry %q does not exist", args[0])}
		}
		if _, err := loaded.Store.DeleteSkillRegistry(args[0]); err != nil {
			return err
		}
		fmt.Fprintf(r.stdout, "Skill registry %s deleted.\n", args[0])
		return nil
	}})
	return command
}

func (r *runtime) skillsRemoteCommand(ctx context.Context) *cobra.Command {
	return &cobra.Command{Use: "remote <registry> [name]", Short: "list verified entries from a remote Skill registry", Args: cobra.RangeArgs(1, 2), RunE: func(_ *cobra.Command, args []string) error {
		loaded, _, err := r.loadManager()
		if err != nil {
			return err
		}
		registry, err := distributionRegistry(args[0], loaded.Config)
		if err != nil {
			return err
		}
		index, err := registry.Fetch(ctx)
		if err != nil {
			return &protocol.Error{Code: protocol.ErrNetwork, Message: err.Error(), Cause: err}
		}
		entries := index.Skills
		if len(args) == 2 {
			entries = nil
			for _, entry := range index.Skills {
				if entry.Name == args[1] {
					entries = append(entries, entry)
				}
			}
		}
		if r.output != "text" {
			return json.NewEncoder(r.stdout).Encode(map[string]any{"registry": args[0], "skills": entries})
		}
		for _, entry := range entries {
			fmt.Fprintf(r.stdout, "%s\t%s\tkey=%s\t%s\n", entry.Name, entry.Version, entry.KeyID, entry.Description)
		}
		return nil
	}}
}

func (r *runtime) skillsInstallCommand(ctx context.Context) *cobra.Command {
	var scope, version string
	var force bool
	command := &cobra.Command{Use: "install <registry>/<name>", Short: "install a signed remote Skill", Args: cobra.ExactArgs(1), RunE: func(_ *cobra.Command, args []string) error {
		registryName, skillName, ok := strings.Cut(args[0], "/")
		if !ok || registryName == "" || skillName == "" || strings.Contains(skillName, "/") {
			return &protocol.Error{Code: protocol.ErrConfig, Message: "Skill reference must be <registry>/<name>"}
		}
		loaded, _, err := r.loadManager()
		if err != nil {
			return err
		}
		registry, err := distributionRegistry(registryName, loaded.Config)
		if err != nil {
			return err
		}
		entry, err := registry.Select(ctx, skillName, version)
		if err != nil {
			return &protocol.Error{Code: protocol.ErrNetwork, Message: err.Error(), Cause: err}
		}
		installation, err := skilldist.Install(ctx, registry, entry, skilldist.InstallOptions{Scope: scope, Workspace: loaded.Workspace, Force: force})
		if err != nil {
			return &protocol.Error{Code: protocol.ErrTool, Message: "install Skill: " + err.Error(), Cause: err}
		}
		return r.printInstallation("Installed", installation)
	}}
	command.Flags().StringVar(&scope, "scope", skilldist.ScopeUser, "installation scope: user, project, or team")
	command.Flags().StringVar(&version, "version", "", "semantic version; latest when empty")
	command.Flags().BoolVar(&force, "force", false, "replace an existing unmanaged Skill directory")
	return command
}

func (r *runtime) skillsUpdateCommand(ctx context.Context) *cobra.Command {
	var scope string
	command := &cobra.Command{Use: "update <name>", Short: "update a signed installed Skill", Args: cobra.ExactArgs(1), RunE: func(_ *cobra.Command, args []string) error {
		loaded, _, err := r.loadManager()
		if err != nil {
			return err
		}
		target, err := skilldist.Target(scope, loaded.Workspace, "", args[0])
		if err != nil {
			return &protocol.Error{Code: protocol.ErrConfig, Message: err.Error()}
		}
		manifest, err := skilldist.LoadManifest(target)
		if err != nil {
			return &protocol.Error{Code: protocol.ErrConfig, Message: "load Skill installation: " + err.Error(), Cause: err}
		}
		registry, err := distributionRegistry(manifest.Registry, loaded.Config)
		if err != nil {
			return err
		}
		entry, changed, err := skilldist.LatestUpdate(ctx, registry, manifest)
		if err != nil {
			return &protocol.Error{Code: protocol.ErrNetwork, Message: err.Error(), Cause: err}
		}
		if !changed {
			fmt.Fprintf(r.stdout, "Skill %s is current at %s.\n", args[0], manifest.Entry.Version)
			return nil
		}
		installation, err := skilldist.Install(ctx, registry, entry, skilldist.InstallOptions{Scope: scope, Workspace: loaded.Workspace})
		if err != nil {
			return &protocol.Error{Code: protocol.ErrTool, Message: "update Skill: " + err.Error(), Cause: err}
		}
		return r.printInstallation("Updated", installation)
	}}
	command.Flags().StringVar(&scope, "scope", skilldist.ScopeUser, "installation scope: user, project, or team")
	return command
}

func (r *runtime) skillsVerifyInstalledCommand() *cobra.Command {
	var scope string
	command := &cobra.Command{Use: "verify <name>", Short: "verify an installed Skill signature and tree digest", Args: cobra.ExactArgs(1), RunE: func(_ *cobra.Command, args []string) error {
		loaded, _, err := r.loadManager()
		if err != nil {
			return err
		}
		target, err := skilldist.Target(scope, loaded.Workspace, "", args[0])
		if err != nil {
			return err
		}
		installation, err := skilldist.VerifyDirectory(target, loaded.Config.SkillRegistries)
		if err != nil {
			return &protocol.Error{Code: protocol.ErrTool, Message: "verify Skill: " + err.Error(), Cause: err}
		}
		return r.printInstallation("Verified", installation)
	}}
	command.Flags().StringVar(&scope, "scope", skilldist.ScopeUser, "installation scope: user, project, or team")
	return command
}

func distributionRegistry(name string, cfg config.Config) (*skilldist.Registry, error) {
	registryConfig, ok := cfg.SkillRegistries[name]
	if !ok || registryConfig.Disabled {
		return nil, &protocol.Error{Code: protocol.ErrConfig, Message: fmt.Sprintf("enabled Skill registry %q was not found", name)}
	}
	return skilldist.NewRegistry(name, registryConfig, nil), nil
}

func (r *runtime) printInstallation(action string, installation skilldist.Installation) error {
	if r.output != "text" {
		return json.NewEncoder(r.stdout).Encode(map[string]any{"action": strings.ToLower(action), "installation": installation})
	}
	fmt.Fprintf(r.stdout, "%s Skill %s %s at %s (scope=%s digest=%s).\n", action, installation.Name, installation.Version, installation.Path, installation.Scope, installation.Digest)
	return nil
}
