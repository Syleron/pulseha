// PulseHA - HA Cluster Daemon
// Copyright (C) 2017-2021  Andrew Zak <andrew@linux.com>
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published
// by the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <http://www.gnu.org/licenses/>.

package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"github.com/syleron/pulseha/internal/cli"
	"github.com/syleron/pulseha/packages/utils"
)

func main() {
	if err := rootCmd.Execute(); err != nil {
		// Printed here, once: cobra's own printing is silenced on the root, which
		// had every failure printed twice (GitHub #19).
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}

var (
	versionFlag bool
	rootCmd     = &cobra.Command{
		Use:   "pulsectl",
		Short: "PulseHA cluster management tool",
		Long:  `PulseHA cluster management tool - Manage your high availability cluster`,
		// main prints a returned error itself, so cobra must not print it as well.
		SilenceErrors: true,
		// A command that fails while running — the daemon is unreachable, it refused
		// the change — is not a usage mistake, and printing the usage after it read
		// as if the operator had typed the command wrong (GitHub #19). So usage is
		// silenced here, once the command line has been accepted: flags are parsed by
		// now, and required flags are checked first because cobra only checks them
		// after this hook, so a missing one still shows the usage. Inherited by every
		// subcommand, none of which sets its own.
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			if err := cmd.ValidateRequiredFlags(); err != nil {
				return err
			}
			cmd.SilenceUsage = true
			return nil
		},
		Run: func(cmd *cobra.Command, args []string) {
			if versionFlag {
				fmt.Printf("PulseHA CLI Version: %s\n", utils.Version)
				fmt.Printf("Build: %s\n", utils.Build)
				os.Exit(0)
			}
			// If no flags or subcommands, show help
			cmd.Help()
		},
	}
)

func init() {
	rootCmd.Flags().BoolVarP(&versionFlag, "version", "v", false, "Show version information")

	rootCmd.AddCommand(&cobra.Command{
		Use:   "version",
		Short: "Show version information",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Printf("PulseHA CLI Version: %s\n", utils.Version)
			fmt.Printf("Build: %s\n", utils.Build)
		},
	})

	rootCmd.AddCommand(
		cli.NewClusterCmd(),
		cli.NewNodeCmd(),
		cli.NewGroupCmd(),
		cli.NewStatusCmd(),
		cli.NewConfigCmd(), // Add config command
	)
}
