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
	"fmt"
	log "github.com/Sirupsen/logrus"
	"sync"
	"time"
	"strings"
	"github.com/Syleron/PulseHA/src/server"
	"github.com/Syleron/PulseHA/src/plugins"
		"github.com/Syleron/PulseHA/src/config"
)

var (
	Version string
	Build   string
)

var pulse *Pulse

/**
 * Main Pulse struct type
 */
type Pulse struct {
	Server *server.Server
	CLI    *server.CLIServer
	Plugins *plugins.Plugins
}

func (p *Pulse) getMemberlist() (*server.Memberlist) {
	return pulse.Server.Memberlist
}

type PulseLogFormat struct {}


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
	// New instance of config
	config := &config.Config{}
	// Load the config
	config.Load()
	// Validate the config
	config.Validate()
	// Define new Member list
	memberList := &server.Memberlist{}
	// Create the Pulse object
	pulse := &Pulse{
		Server: &server.Server{
			Memberlist: memberList,
		},
		CLI: &server.CLIServer{
			Memberlist: memberList,
		},
		Plugins: &plugins.Plugins{},
	}
	// Set our server variable.. should this be a reference?
	pulse.CLI.Server = pulse.Server
	return pulse
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
	// Load plugins
	pulse.Plugins.Setup()
	// Setup wait group
	var wg sync.WaitGroup
	wg.Add(1)
	// Setup cli
	go pulse.CLI.Setup()
	// Setup server
	go pulse.Server.Setup()
	wg.Wait()
}
