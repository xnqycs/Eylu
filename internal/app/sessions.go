package app

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"Eylu/internal/protocol"
	"Eylu/internal/session"
)

func (r *runtime) sessionsCommand() *cobra.Command {
	command := &cobra.Command{Use: "sessions", Short: "list, inspect, migrate, and remove saved sessions"}
	command.AddCommand(r.sessionsListCommand(), r.sessionsShowCommand(), r.sessionsDeleteCommand(), r.sessionsMigrateCommand(), r.sessionsCleanupCommand())
	return command
}

func (r *runtime) sessionsListCommand() *cobra.Command {
	return &cobra.Command{Use: "list", Short: "list saved sessions", Args: cobra.NoArgs, RunE: func(*cobra.Command, []string) error {
		store, err := session.Open("")
		if err != nil {
			return sessionProtocolError("open session store", err)
		}
		items, err := store.List()
		if err != nil {
			return sessionProtocolError("list sessions", err)
		}
		if r.output != "text" {
			return json.NewEncoder(r.stdout).Encode(map[string]any{"sessions": items})
		}
		if len(items) == 0 {
			fmt.Fprintln(r.stdout, "No saved sessions.")
			return nil
		}
		for _, item := range items {
			status := "open"
			if item.ClosedAt != nil {
				status = "closed"
			}
			fmt.Fprintf(r.stdout, "%s\t%s\t%s\t%s\t%s\tturns=%d\tbytes=%d\t%s\n", item.SessionID, status, item.Mode, item.Provider, item.Model, item.Turns, item.Bytes, item.UpdatedAt.Format("2006-01-02T15:04:05Z07:00"))
			if item.Diagnostic != "" {
				fmt.Fprintf(r.stdout, "  diagnostic: %s\n", item.Diagnostic)
			}
		}
		return nil
	}}
}

func (r *runtime) sessionsShowCommand() *cobra.Command {
	return &cobra.Command{Use: "show <id>", Short: "show a saved session and transcript", Args: cobra.ExactArgs(1), RunE: func(_ *cobra.Command, args []string) error {
		store, err := session.Open("")
		if err != nil {
			return sessionProtocolError("open session store", err)
		}
		snapshot, diagnostics, err := store.Load(args[0])
		if err != nil {
			return sessionProtocolError("load session", err)
		}
		if r.output != "text" {
			return json.NewEncoder(r.stdout).Encode(map[string]any{"session": snapshot, "diagnostics": diagnostics})
		}
		status := "open"
		if snapshot.ClosedAt != nil {
			status = "closed"
		}
		fmt.Fprintf(r.stdout, "Session: %s\nStatus: %s\nWorkspace: %s\nMode: %s\nProvider: %s generation=%d\nModel: %s\nTurns: %d\nUpdated: %s\n", snapshot.SessionID, status, snapshot.Workspace, snapshot.PermissionMode, snapshot.Provider.Name, snapshot.Provider.Generation, snapshot.Provider.Model, len(snapshot.Turns), snapshot.UpdatedAt.Format("2006-01-02T15:04:05Z07:00"))
		for _, diagnostic := range diagnostics {
			fmt.Fprintf(r.stdout, "Diagnostic: %s: %s\n", diagnostic.Path, diagnostic.Message)
		}
		for _, turn := range snapshot.Turns {
			data, marshalErr := json.Marshal(turn.Parts)
			if marshalErr != nil {
				return marshalErr
			}
			fmt.Fprintf(r.stdout, "\n[%s] %s %s\n", turn.Role, turn.ID, data)
		}
		return nil
	}}
}

func (r *runtime) sessionsDeleteCommand() *cobra.Command {
	var approve bool
	command := &cobra.Command{Use: "delete <id>", Short: "delete a session after confirmation", Args: cobra.ExactArgs(1), RunE: func(_ *cobra.Command, args []string) error {
		id := args[0]
		if !session.ValidID(id) {
			return &protocol.Error{Code: protocol.ErrConfig, Message: fmt.Sprintf("invalid session ID %q", id)}
		}
		if !approve {
			if !isTerminal(r.stdin) {
				return &protocol.Error{Code: protocol.ErrConfig, Message: "session deletion requires confirmation; pass --yes in non-interactive use"}
			}
			fmt.Fprintf(r.stderr, "Delete session %s and its attachments? [y/N]: ", id)
			reader := r.inputReader
			if reader == nil {
				reader = bufio.NewReader(r.stdin)
			}
			answer, err := reader.ReadString('\n')
			if err != nil && !errors.Is(err, io.EOF) {
				return err
			}
			if !strings.EqualFold(strings.TrimSpace(answer), "y") && !strings.EqualFold(strings.TrimSpace(answer), "yes") {
				fmt.Fprintln(r.stdout, "Deletion cancelled.")
				return nil
			}
		}
		store, err := session.Open("")
		if err != nil {
			return sessionProtocolError("open session store", err)
		}
		if err := store.Delete(id); err != nil {
			return sessionProtocolError("delete session", err)
		}
		fmt.Fprintf(r.stdout, "Deleted session %s.\n", id)
		return nil
	}}
	command.Flags().BoolVarP(&approve, "yes", "y", false, "confirm session deletion")
	return command
}

func (r *runtime) sessionsMigrateCommand() *cobra.Command {
	return &cobra.Command{Use: "migrate <id>", Short: "migrate a legacy session schema", Args: cobra.ExactArgs(1), RunE: func(_ *cobra.Command, args []string) error {
		store, err := session.Open("")
		if err != nil {
			return sessionProtocolError("open session store", err)
		}
		if err := store.Migrate(args[0]); err != nil {
			return sessionProtocolError("migrate session", err)
		}
		fmt.Fprintf(r.stdout, "Migrated session %s to schema %d.\n", args[0], session.SchemaVersion)
		return nil
	}}
}

func (r *runtime) sessionsCleanupCommand() *cobra.Command {
	return &cobra.Command{Use: "cleanup", Short: "apply configured session retention limits", Args: cobra.NoArgs, RunE: func(*cobra.Command, []string) error {
		loaded, _, err := r.loadManager()
		if err != nil {
			return err
		}
		store, err := session.Open("")
		if err != nil {
			return sessionProtocolError("open session store", err)
		}
		deleted, err := store.Cleanup(loaded.Config.MaxSessions, loaded.Config.MaxSessionBytes, "")
		if err != nil {
			return sessionProtocolError("clean sessions", err)
		}
		if r.output != "text" {
			return json.NewEncoder(r.stdout).Encode(map[string]any{"deleted": deleted})
		}
		fmt.Fprintf(r.stdout, "Removed %d sessions.\n", len(deleted))
		return nil
	}}
}
