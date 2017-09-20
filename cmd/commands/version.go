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
	"bytes"
	"fmt"
	"github.com/mitchellh/cli"
)

type VersionCommand struct {
	Version        string
	VersionRelease string
	Ui             cli.Ui
}

/**
 *
 */
func (c *VersionCommand) Help() string {
	return ""
}

/**
 *
 */
func (c *VersionCommand) Run(_ []string) int {
	var versionString bytes.Buffer

	fmt.Fprintf(&versionString, "pulseha v%s", c.Version)

	c.Ui.Output(versionString.String() + " Copyright (c) 2017 Andrew Zak <andrew@pulseha.com>")

	return 0
}

/**
 *
 */
func (c *VersionCommand) Synopsis() string {
	return "Prints the PulseHA version"
}
