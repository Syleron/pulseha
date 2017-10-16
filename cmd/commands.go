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
package main

import (
	"github.com/Syleron/PulseHA/cmd/commands"
	"github.com/mitchellh/cli"
	"os"
	"os/signal"
)

var Commands map[string]cli.CommandFactory

/**
 *
 */
func init() {
	ui := &cli.BasicUi{Writer: os.Stdout}

	Commands = map[string]cli.CommandFactory{
		"join": func() (cli.Command, error) {
			return &commands.JoinCommand{
				Ui: ui,
			}, nil
		},
		"create": func() (cli.Command, error) {
			return &commands.CreateCommand{
				Ui: ui,
			}, nil
		},
		"groups": func() (cli.Command, error) {
			return &commands.GroupsCommand{
				Ui: ui,
			}, nil
		},
		"leave": func() (cli.Command, error) {
			return &commands.LeaveCommand{
				Ui: ui,
			}, nil
		},
		"status": func() (cli.Command, error) {
			return &commands.StatusCommand{
				Ui: ui,
			}, nil
		},
		"promote": func() (cli.Command, error) {
			return &commands.PromoteCommand{
				Ui: ui,
			}, nil
		},
		"version": func() (cli.Command, error) {
			return &commands.VersionCommand{
				Version:        Version,
				Build:          Build,
				VersionRelease: VersionRelease,
				Ui:             ui,
			}, nil
		},
	}
}

/**
 *
 */
func makeShutdownCh() <-chan struct{} {
	resultCh := make(chan struct{})

	signalCh := make(chan os.Signal, 4)
	signal.Notify(signalCh, os.Interrupt)
	go func() {
		for {
			<-signalCh
			resultCh <- struct{}{}
		}
	}()

	return resultCh
}
