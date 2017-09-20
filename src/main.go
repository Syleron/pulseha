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
	"github.com/coreos/go-log/log"
	"os"
	"sync"
	"fmt"
)

/**
 * Main Pulse struct type
 */
type Pulse struct {
	Client *Client
	Server *Server
	Config *Config
}

/**
 * Create a new instance of PulseHA
 */
func createPulse() (*Pulse) {
	config := &Config{}
	// Load the config
	config.Load()
	// Validate the config
	config.Validate()
	// Create the Pulse object
	pulse := &Pulse{
		Server: &Server{
			Config: config,
		},
		Client: &Client{
			Config: config,
		},
		Config: config,
	}
	return pulse
}

/**
 * Essential Construct
 */
func main() {
	// Draw logo
	fmt.Println(`
   ___       _                  _
  / _ \_   _| |___  ___  /\  /\/_\
 / /_)/ | | | / __|/ _ \/ /_/ //_\\
/ ___/| |_| | \__ \  __/ __  /  _  \
\/     \__,_|_|___/\___\/ /_/\_/ \_/  Version 0.0.1
	`)
	pulse := createPulse()
	// Load plugins
	_, err := LoadPlugins()
	if err != nil {
		log.Errorf("Failed to load plugins: %s", err)
		os.Exit(1)
	}
	var wg sync.WaitGroup
	wg.Add(1)
	// Setup cli
	go pulse.Server.SetupCLI()
	// Setup go routine for client
	go pulse.Client.Setup()
	// Setup server
	go pulse.Server.Setup()
	wg.Wait()
}
