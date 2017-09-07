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
  Tells a running PulseHA agent to join the cluster
  by specifying at least one existing member.
`
	return strings.TrimSpace(helpText)
}

func (c *JoinCommand) Run(args []string) int {
	cmdFlags := flag.NewFlagSet("join", flag.ContinueOnError)
	cmdFlags.Usage = func() { c.Ui.Output(c.Help()) }

	addr := cmdFlags.Args()

	if len(addr) == 0 {
		// No address was specified... error out
		c.Ui.Error("Please specify an address to join.")
		c.Ui.Error("")
		c.Ui.Error(c.Help())
		return 1
	}

	return 0
}

func (c *JoinCommand) Synopsis() string {
	return "Tell Pulse to join a cluster"
}
