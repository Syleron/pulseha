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

package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"github.com/syleron/pulseha/internal/client"
	rpc "github.com/syleron/pulseha/rpc"
)

// NewConfigCmd creates the config command
func NewConfigCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Manage configuration settings",
		Long:  `View and modify PulseHA configuration settings`,
	}

	cmd.AddCommand(
		newConfigAllCmd(),
		newConfigGetCmd(),
		newConfigSetCmd(),
	)

	return cmd
}

// newConfigAllCmd creates the config all command
func newConfigAllCmd() *cobra.Command {
	var jsonOutput bool

	cmd := &cobra.Command{
		Use:   "all",
		Short: "Display all global configuration settings",
		Long:  `Display all global PulseHA configuration settings retrieved live from the daemon.`,
		Args:  cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			c, err := client.New()
			if err != nil {
				fmt.Printf("Error: %v\n", err)
				os.Exit(1)
			}
			defer c.Connection.Close()

			cfg, err := c.GetConfig()
			if err != nil {
				fmt.Printf("Error: %v\n", err)
				os.Exit(1)
			}

			if jsonOutput {
				data, err := json.MarshalIndent(cfg.Pulse, "", "  ")
				if err != nil {
					fmt.Printf("Error: %v\n", err)
					os.Exit(1)
				}
				fmt.Println(string(data))
				return
			}

			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "Global Configuration:")
			fmt.Fprintln(w, "====================")
			fmt.Fprintf(w, "Mode:\t%s\n", cfg.Pulse.Mode)
			fmt.Fprintf(w, "Logging Level:\t%s\n", cfg.Pulse.LoggingLevel)
			fmt.Fprintf(w, "Health Check Interval:\t%dms\n", cfg.Pulse.HealthCheckInterval)
			fmt.Fprintf(w, "Failover Interval:\t%dms\n", cfg.Pulse.FailOverInterval)
			fmt.Fprintf(w, "Failover Limit:\t%dms\n", cfg.Pulse.FailOverLimit)
			fmt.Fprintf(w, "Auto Failback:\t%v\n", cfg.Pulse.AutoFailback)
			fmt.Fprintf(w, "Log to File:\t%v\n", cfg.Pulse.LogToFile)
			fmt.Fprintf(w, "Log File Location:\t%s\n", cfg.Pulse.LogFileLocation)
			fmt.Fprintf(w, "Log to Syslog:\t%v\n", cfg.Pulse.LogToSyslog)
			fmt.Fprintf(w, "Syslog Network:\t%s\n", cfg.Pulse.SyslogNetwork)
			fmt.Fprintf(w, "Syslog Address:\t%s\n", cfg.Pulse.SyslogAddress)
			fmt.Fprintf(w, "Syslog Facility:\t%s\n", cfg.Pulse.SyslogFacility)
			fmt.Fprintf(w, "Syslog Tag:\t%s\n", cfg.Pulse.SyslogTag)
			w.Flush()
		},
	}

	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Output in JSON format")
	return cmd
}

// newConfigGetCmd creates the config get command
func newConfigGetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "get [key]",
		Short: "Get configuration value(s)",
		Long:  `Get configuration value(s). If no key is specified, all values are shown.`,
		Args:  cobra.MaximumNArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			// Connect to local server
			c, err := client.New()
			if err != nil {
				fmt.Printf("Error: %v\n", err)
				os.Exit(1)
			}
			defer c.Connection.Close()

			// Get live config from daemon via gRPC
			cfg, err := c.GetConfig()
			if err != nil {
				fmt.Printf("Error: %v\n", err)
				os.Exit(1)
			}

			if len(args) == 0 {
				// Show all config values
				w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
				fmt.Fprintln(w, "Configuration Settings:")
				fmt.Fprintln(w, "======================")
				fmt.Fprintf(w, "Mode:\t%s\n", cfg.Pulse.Mode)
				fmt.Fprintf(w, "Logging Level:\t%s\n", cfg.Pulse.LoggingLevel)
				fmt.Fprintf(w, "Log to File:\t%v\n", cfg.Pulse.LogToFile)
				fmt.Fprintf(w, "Log to Syslog:\t%v\n", cfg.Pulse.LogToSyslog)
				fmt.Fprintf(w, "Health Check Interval:\t%dms\n", cfg.Pulse.HealthCheckInterval)
				fmt.Fprintf(w, "Failover Interval:\t%dms\n", cfg.Pulse.FailOverInterval)
				fmt.Fprintf(w, "Failover Limit:\t%dms\n", cfg.Pulse.FailOverLimit)
				fmt.Fprintf(w, "Auto Failback:\t%v\n", cfg.Pulse.AutoFailback)
				w.Flush()
			} else {
				// Show specific config value
				key := args[0]
				switch key {
				case "mode":
					fmt.Println(cfg.Pulse.Mode)
				case "logging_level":
					fmt.Println(cfg.Pulse.LoggingLevel)
				case "log_to_file":
					fmt.Println(cfg.Pulse.LogToFile)
				case "log_to_syslog":
					fmt.Println(cfg.Pulse.LogToSyslog)
				case "hcs_interval":
					fmt.Println(cfg.Pulse.HealthCheckInterval)
				case "fos_interval":
					fmt.Println(cfg.Pulse.FailOverInterval)
				case "fo_limit":
					fmt.Println(cfg.Pulse.FailOverLimit)
				case "auto_failback":
					fmt.Println(cfg.Pulse.AutoFailback)
				default:
					// Try to get it as JSON field
					data, _ := json.Marshal(cfg.Pulse)
					var m map[string]interface{}
					json.Unmarshal(data, &m)
					if val, ok := m[key]; ok {
						fmt.Println(val)
					} else {
						fmt.Printf("Error: unknown config key: %s\n", key)
						os.Exit(1)
					}
				}
			}
		},
	}
}

// newConfigSetCmd creates the config set command
func newConfigSetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "set <key> <value>",
		Short: "Set configuration value",
		Long: `Set a configuration value.

Cluster-wide keys are applied to every node:
  mode, hcs_interval, fos_interval, fo_limit, auto_failback

The timing keys (hcs_interval, fos_interval, fo_limit) are read when the health
checker starts, so a change to one of them takes effect on the next restart of
each daemon. mode takes effect immediately.

Logging keys apply to the node the command runs on only, and have to be set on
each node individually:
  logging_level, log_to_file, log_file_location, log_to_syslog,
  syslog_network, syslog_address, syslog_facility, syslog_tag

The reach of each change is reported back on success.`,
		Args: cobra.ExactArgs(2),
		Run: func(cmd *cobra.Command, args []string) {
			key := args[0]
			value := args[1]

			// Connect to server
			c, err := client.New()
			if err != nil {
				fmt.Printf("Error: %v\n", err)
				os.Exit(1)
			}
			defer c.Connection.Close()

			// Send update request
			ctx := context.Background()
			resp, err := c.CLI().UpdateConfig(ctx, &rpc.UpdateConfigRequest{
				Key:   key,
				Value: value,
			})
			if err != nil {
				fmt.Printf("Error: %v\n", err)
				os.Exit(1)
			}

			if resp.Success {
				// The daemon reports the reach of the change; print it rather than
				// leaving the operator to infer it from the help text, which used to
				// promise the cluster for a value that reached one node.
				if resp.Message != "" {
					fmt.Printf("Successfully updated %s to %s (%s)\n", key, value, resp.Message)
				} else {
					fmt.Printf("Successfully updated %s to %s\n", key, value)
				}
			} else {
				fmt.Printf("Error: %s\n", resp.Message)
				os.Exit(1)
			}
		},
	}
}
