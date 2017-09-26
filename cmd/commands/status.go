/*
    PulseHA - HA Cluster Daemon
    Copyright (C) 2017  Andrew Zak <andrew@pulseha.com>

    This program is free software: you can redistribute it and/or modify
    it under the terms of the GNU Affero General Public License as published
    by the Free Software Foundation, either version 3 of the License, or
    (at your option) any later version.

    This program is distributed in the hope that it will be useful,
    but WITHOUT ANY WARRANTY; without even the implied warranty of
    MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
    GNU Affero General Public License for more details.

    You should have received a copy of the GNU Affero General Public License
    along with this program.  If not, see <http://www.gnu.org/licenses/>.
 */
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
