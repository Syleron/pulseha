package commands

import (
	"github.com/mitchellh/cli"
	"strings"
	"flag"
)

type GroupCommand struct {
	Ui cli.Ui
}

/**
 *
 */
func (c *GroupCommand) Help() string {
	helpText := `
Usage: pulseha group [options] ...
  Tells a running PulseHA agent to join the cluster
  by specifying at least one existing member.
Options:
`
	return strings.TrimSpace(helpText)
}

/**
 *
 */
func (c *GroupCommand) Run(args []string) int {
	cmdFlags := flag.NewFlagSet("group", flag.ContinueOnError)
	cmdFlags.Usage = func() { c.Ui.Output(c.Help()) }

	return 0
}

/**
 *
 */
func (c *GroupCommand) Synopsis() string {
	return ""
}
