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

package main

import (
	"fmt"
	"github.com/mitchellh/cli"
	"github.com/syleron/pulseha/src/pulsectl"
	"io/ioutil"
	"log"
	"os"
)

var (
	Commands map[string]cli.CommandFactory

	Version string
	Build   string
)

/**
 *
 */
func main() {
	os.Exit(realMain())
}

/**
 *
 */
func realMain() int {
	log.SetOutput(ioutil.Discard)

	args := os.Args[1:]
	for _, arg := range args {
		if arg == "-v" || arg == "--version" {
			newArgs := make([]string, len(args)+1)
			newArgs[0] = "version"
			copy(newArgs[1:], args)
			args = newArgs
			break
		}
	}

	cli := &cli.CLI{
		Name:         "pulsectl",
		Args:         args,
		Commands:     Commands,
		Autocomplete: true,
		HelpFunc:     cli.BasicHelpFunc("pulsectl"),
	}

	exitCode, err := cli.Run()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error executing CLI: %s\n", err.Error())
		return 1
	}

	return exitCode
}

/**
 *
 */
func init() {
	ui := &cli.BasicUi{Writer: os.Stdout}

	Commands = map[string]cli.CommandFactory{
		"join": func() (cli.Command, error) {
			return &pulsectl.JoinCommand{
				Ui: ui,
			}, nil
		},
		"create": func() (cli.Command, error) {
			return &pulsectl.CreateCommand{
				Ui: ui,
			}, nil
		},
		"groups": func() (cli.Command, error) {
			return &pulsectl.GroupsCommand{
				Ui: ui,
			}, nil
		},
		"leave": func() (cli.Command, error) {
			return &pulsectl.LeaveCommand{
				Ui: ui,
			}, nil
		},
		"remove": func() (cli.Command, error) {
			return &pulsectl.RemoveCommand{
				Ui: ui,
			}, nil
		},
		"status": func() (cli.Command, error) {
			return &pulsectl.StatusCommand{
				Ui: ui,
			}, nil
		},
		"promote": func() (cli.Command, error) {
			return &pulsectl.PromoteCommand{
				Ui: ui,
			}, nil
		},
		"cert": func() (cli.Command, error) {
			return &pulsectl.CertCommand{
				Ui: ui,
			}, nil
		},
		"token": func() (cli.Command, error) {
			return &pulsectl.TokenCommand{
				Ui: ui,
			}, nil
		},
		"config": func() (cli.Command, error) {
			return &pulsectl.ConfigCommand{
				Ui: ui,
			}, nil
		},
		"network": func() (cli.Command, error) {
			return &pulsectl.NetworkCommand{
				Ui: ui,
			}, nil
		},
		"version": func() (cli.Command, error) {
			return &pulsectl.VersionCommand{
				Version: Version,
				Build:   Build,
				Ui:      ui,
			}, nil
		},
	}
}
