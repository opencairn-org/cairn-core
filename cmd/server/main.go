// Command cairn is the single binary for the Cairn server.
//
// Subcommands:
//
//	cairn serve              start the HTTP / Connect-RPC server
//	cairn migrate up         apply pending migrations
//	cairn migrate down       roll back the most recent migration
//	cairn migrate down-to V  roll back to version V
//	cairn migrate redo       roll back and re-apply the latest migration
//	cairn migrate status     list each migration's applied state
//	cairn migrate version    print the current applied migration version
//	cairn version            print binary version metadata
//	cairn config help        print the full environment-variable surface
//
// All configuration is taken from environment variables prefixed CAIRN_.
// See internal/config for the full schema.
package main

import (
	"fmt"
	"os"

	// Embed the IANA timezone database in the binary so time.LoadLocation works
	// regardless of the base image (slim/distroless containers ship no tzdata).
	// Used by notification quiet hours, which interpret the window in the user's
	// IANA timezone.
	_ "time/tzdata"

	"github.com/spf13/cobra"

	"github.com/opencairn-org/cairn-core/internal/config"
)

func main() {
	if err := newRootCmd().Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "cairn: %v\n", err)
		os.Exit(1)
	}
}

// newRootCmd assembles the cairn CLI.
func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "cairn",
		Short:         "Cairn — self-hosted activity tracker",
		SilenceUsage:  true, // don't dump usage on every runtime error
		SilenceErrors: true, // we print errors ourselves with consistent formatting
	}

	root.AddCommand(newServeCmd())
	root.AddCommand(newMigrateCmd())
	root.AddCommand(newVersionCmd())
	root.AddCommand(newConfigCmd())
	root.AddCommand(newRotateKeyCmd())
	root.AddCommand(newGenSecretsCmd())
	root.AddCommand(newAttachmentsCmd())
	return root
}

// newConfigCmd hosts the small `config help` helper. Lives here rather
// than in its own file because the body is trivial.
func newConfigCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Configuration inspection",
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "help",
		Short: "Print every CAIRN_* environment variable, default, and required state",
		RunE: func(*cobra.Command, []string) error {
			return config.PrintHelp()
		},
	})
	return cmd
}
