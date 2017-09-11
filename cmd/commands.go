package main

import (
	"os"
	"os/signal"
	"github.com/mitchellh/cli"
	"github.com/Syleron/Pulse/cmd/commands"
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
		"group": func() (cli.Command, error) {
			return &commands.GroupCommand{
				Ui: ui,
			}, nil
		},
		"version": func() (cli.Command, error) {
			return &commands.VersionCommand{
				Version:        Version,
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
