package commands

import (
	"strings"
	"flag"
	"github.com/mitchellh/cli"
)

type StatusCommand struct {
	Ui cli.Ui
}

/**
 *
 */
func (c *StatusCommand) Help() string {
	helpText := `
Usage: pulseha status [options] ...
Options:
`
	return strings.TrimSpace(helpText)
}

/**
 *
 */
func (c *StatusCommand) Run(args []string) int {
	cmdFlags := flag.NewFlagSet("status", flag.ContinueOnError)
	cmdFlags.Usage = func() { c.Ui.Output(c.Help()) }

	return 0
}

/**
 *
 */
func (c *StatusCommand) Synopsis() string {
	return "Provides a status overview of your configured cluster"
}
