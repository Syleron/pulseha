package commands

import (
	"github.com/mitchellh/cli"
	"strings"
	"flag"
)

type CreateCommand struct {
	Ui cli.Ui
}

func (c *CreateCommand) Help() string {
	helpText := `
Usage: PulseHA create [options] address ...
  Tells a running PulseHA agent (with "pulseha agent") to join the cluster
  by specifying at least one existing member.
Options:
`
	return strings.TrimSpace(helpText)
}

func (c *CreateCommand) Run(args []string) int {
	cmdFlags := flag.NewFlagSet("create", flag.ContinueOnError)
	cmdFlags.Usage = func() { c.Ui.Output(c.Help()) }

	return 0
}

func (c *CreateCommand) Synopsis() string {
	return "Tell Pulse to create new HA cluster"
}
