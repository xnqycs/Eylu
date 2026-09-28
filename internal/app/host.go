package app

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"Eylu/internal/host"
)

func (r *runtime) serveCommand(ctx context.Context) *cobra.Command {
	var transport, protocolVersion string
	cmd := &cobra.Command{Use: "serve", Short: "serve the isolated Bastion host protocol over stdio", Args: func(cmd *cobra.Command, args []string) error { r.output = "text"; return cobra.NoArgs(cmd, args) },
		PreRunE: func(cmd *cobra.Command, _ []string) error {
			r.output = "text" // Startup diagnostics also stay off protocol stdout.
			if transport != "stdio" || protocolVersion != host.ProtocolVersion {
				return fmt.Errorf("serve requires --transport stdio --protocol %s", host.ProtocolVersion)
			}
			for _, name := range []string{"config", "workspace", "output"} {
				if cmd.Flags().Changed(name) {
					return fmt.Errorf("serve does not accept --%s", name)
				}
			}
			return nil
		},
		RunE: func(*cobra.Command, []string) error { return host.Serve(ctx, r.stdin, r.stdout, r.stderr) },
	}
	cmd.Flags().StringVar(&transport, "transport", "stdio", "transport (stdio only)")
	cmd.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { r.output = "text"; return err })
	cmd.Flags().StringVar(&protocolVersion, "protocol", host.ProtocolVersion, "host protocol version")
	return cmd
}
