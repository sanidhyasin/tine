// Command tined is the tine service. It runs on the Linux host and owns every
// microVM. It also has admin commands for the host, such as importing images.
package main

import (
	"errors"
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
		Use:          "tined",
		Short:        "The tine service: runs on the Linux host and owns every microVM",
		Version:      version.String(),
		SilenceUsage: true,
	}
	root.AddCommand(newServeCmd(), newImageCmd(), newVersionCmd())
	return root
}

func newServeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "serve",
		Short: "Run the tine service",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			return errors.New("the service isn't built yet (docs/PLAN.md, task 1.10)")
		},
	}
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the tined version",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, err := fmt.Fprintln(cmd.OutOrStdout(), "tined", version.String())
			return err
		},
	}
}
