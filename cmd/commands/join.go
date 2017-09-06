package commands

import (
	"github.com/mitchellh/cli"
	"strings"
	"flag"
)

type JoinCommand struct {
	Ui cli.Ui
}

func (c *JoinCommand) Help() string {
	helpText := `
Usage: PulseHA join [options] address ...
  Tells a running PulseHA agent (with "pulseha agent") to join the cluster
  by specifying at least one existing member.
Options:
`
	return strings.TrimSpace(helpText)
}

func (c *JoinCommand) Run(args []string) int {
	cmdFlags := flag.NewFlagSet("join", flag.ContinueOnError)
	cmdFlags.Usage = func() { c.Ui.Output(c.Help()) }

	return 0
}

func (c *JoinCommand) Synopsis() string {
	return "Tell Pulse to join a custer"
}
