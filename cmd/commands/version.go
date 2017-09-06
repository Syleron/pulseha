package commands

import (
	"bytes"
	"fmt"
	"github.com/mitchellh/cli"
)

type VersionCommand struct {
	Version        string
	VersionRelease string
	Ui             cli.Ui
}

func (c *VersionCommand) Help() string {
	return ""
}

func (c *VersionCommand) Run(_ []string) int {
	var versionString bytes.Buffer

	fmt.Fprintf(&versionString, "PulseHA v%s", c.Version)

	c.Ui.Output(versionString.String())

	return 0
}

func (c *VersionCommand) Synopsis() string {
	return "Prints the PulseHA version"
}
