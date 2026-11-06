// Command tine is the command-line client for tined, the tine service.
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/sanidhyasin/tine/internal/version"
)

func main() {
	if err := newRootCmd().Execute(); err != nil {
		// Cobra has already printed the error.
		os.Exit(1)
	}
}

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:          "tine",
		Short:        "Fork running microVMs in milliseconds",
		Long:         "tine creates, snapshots and forks Firecracker microVMs through tined, the tine service.",
		Version:      version.String(),
		SilenceUsage: true,
	}
	root.AddCommand(newVersionCmd())
	return root
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the tine version",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, err := fmt.Fprintln(cmd.OutOrStdout(), "tine", version.String())
			return err
		},
	}
}
