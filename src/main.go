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
	"github.com/coreos/go-log/log"
	"os"
	"sync"
)
type globalConfig struct {
	sync.Mutex
	Config
}
/**
 * Returns a copy of the config
 */
func (c *globalConfig)GetConfig()Config{
	return c.Config
}

var (
	Version string
	Build   string
	config  globalConfig
)


/**

 * Main Pulse struct type
 */
type Pulse struct {
	Server *Server
}

/**
 * Create a new instance of PulseHA
 */
func createPulse() *Pulse {
	//config.Config = Config{}
	// Load the config
	config.Load()
	fmt.Println("Hey Andy!")
	fmt.Println(config)
	// Validate the config
	config.Validate()
	// Create the Pulse object
	pulse := &Pulse{
		Server: &Server{
			Memberlist: &Memberlist{
			},
		},
	}
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
	// Setup server
	go pulse.Server.Setup()
	wg.Wait()
}
