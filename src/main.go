/*
   PulseHA - HA Cluster Daemon
   Copyright (C) 2017-2019  Andrew Zak <andrew@linux.com>

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
	"fmt"
	log "github.com/Sirupsen/logrus"
	"github.com/Syleron/PulseHA/src/config"
	"github.com/Syleron/PulseHA/src/logging"
	"github.com/Syleron/PulseHA/src/server"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	PRE_SIGNAL = iota
	POST_SIGNAL
)

var (
	Version         string
	Build           string
	hookableSignals []os.Signal
)

var pulse *Pulse

/**
 * Main Pulse struct type
 */
type Pulse struct {
	DB          server.Database
	Server      *server.Server
	CLI         *server.CLIServer
	Sigs        chan os.Signal
	SignalHooks map[int]map[os.Signal][]func()
}

type PulseLogFormat struct{}

func (f *PulseLogFormat) Format(entry *log.Entry) ([]byte, error) {
	time := "[" + entry.Time.Format(time.Stamp) + "]"
	lvlOut := entry.Level.String()
	switch entry.Level {
	case log.ErrorLevel:
	case log.FatalLevel:
	case log.WarnLevel:
		lvlOut = strings.ToUpper(lvlOut)
	}
	level := "[" + lvlOut + "] "
	message := time + level + entry.Message
	return append([]byte(message), '\n'), nil
}

/**
 * Create a new instance of PulseHA
 */
func createPulse() *Pulse {
	// Create the Pulse object
	pulse := &Pulse{
		Server: &server.Server{},
		CLI:    &server.CLIServer{},
	}
	// Set our server variable
	pulse.CLI.Server = pulse.Server
	return pulse
}

func init() {
	hookableSignals = []os.Signal{
		syscall.SIGHUP,
		syscall.SIGUSR1,
		syscall.SIGUSR2,
		syscall.SIGINT,
		syscall.SIGTERM,
		syscall.SIGTSTP,
	}
}

/**
 * Essential Construct
 */
func main() {
	// Draw logo
	fmt.Printf(`
   ___       _                  _
  / _ \_   _| |___  ___  /\  /\/_\
 / /_)/ | | | / __|/ _ \/ /_/ //_\\
/ ___/| |_| | \__ \  __/ __  /  _  \  Version %s
\/     \__,_|_|___/\___\/ /_/\_/ \_/  Build   %s

`, Version, Build[0:7])
	log.SetFormatter(new(PulseLogFormat))
	pulse = createPulse()
	// listen for singals
	pulse.Sigs = make(chan os.Signal)
	signal.Notify(pulse.Sigs)
	// Handle the signals
	go handleSignals()
	// load the config
	pulse.DB = server.Database{
		Plugins:    &server.Plugins{},
		MemberList: &server.MemberList{},
	}
	// Setup a new pulse Logger
	pulseLogger, err := logging.NewLogger(pulse.DB.MemberList.Broadcast)
	if err != nil {
		panic("unable to create pulseha distributed logger")
	}
	// Set our pulse logger
	pulse.DB.Logging = pulseLogger
	// Load the config
	pulse.DB.Config = config.GetConfig()
	// Set the logging level
	setLogLevel(pulse.DB.Config.Logging.Level)
	// Setup wait group
	var wg sync.WaitGroup
	wg.Add(1)
	// Setup cli
	go pulse.CLI.Setup()
	// Setup server
	go pulse.Server.Init(&pulse.DB)
	wg.Wait()
}

/**

 */
func setLogLevel(level string) {
	logLevel, err := log.ParseLevel(level)
	if err != nil {
		panic(err.Error())
	}
	log.SetLevel(logLevel)
}

/**
Handle OS signals
*/
func handleSignals() {
	var sig os.Signal
	signal.Notify(pulse.Sigs, hookableSignals...)
	for {
		sig = <-pulse.Sigs
		signalHooks(PRE_SIGNAL, sig)
		switch sig {
		case syscall.SIGUSR2:
			// Shutdown our server
			pulse.Server.Shutdown()
			// Reload our config
			pulse.DB.Config.Reload()
			// Start a new server
			go pulse.Server.Setup()
			break
		case syscall.SIGINT:
			fallthrough
		case syscall.SIGTERM:
			// Shutdown our service
			pulse.Server.Shutdown()
			os.Exit(0)
		}
		signalHooks(POST_SIGNAL, sig)
	}
}

/**

 */
func signalHooks(ppFlag int, sig os.Signal) {
	if _, notSet := pulse.SignalHooks[ppFlag][sig]; !notSet {
		return
	}
	for _, f := range pulse.SignalHooks[ppFlag][sig] {
		f()
	}
	return
}
