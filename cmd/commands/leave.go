package commands

import (
"github.com/mitchellh/cli"
"strings"
"flag"
)

type LeaveCommand struct {
	Ui cli.Ui
}

/**
 *
 */
func (c *LeaveCommand) Help() string {
	helpText := `
Usage: PulseHA leave [options] ...
  Tells a running PulseHA agent to join the cluster
  by specifying at least one existing member.
Options:
`
	return strings.TrimSpace(helpText)
}

/**
 *
 */
func (c *LeaveCommand) Run(args []string) int {
	cmdFlags := flag.NewFlagSet("leave", flag.ContinueOnError)
	cmdFlags.Usage = func() { c.Ui.Output(c.Help()) }

	return 0
}

/**
 *
 */
func (c *LeaveCommand) Synopsis() string {
	return "Tell Pulse to create new HA cluster"
}
