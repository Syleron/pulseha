package main

import (
	"fmt"
	"github.com/coreos/go-log/log"
	"os"
	"sync"
)

var (
	Version string
	Build   string
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
func createPulse() *Pulse {
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
	// Setup go routine for client
	go pulse.Client.Setup()
	// Setup server
	go pulse.Server.Setup()
	wg.Wait()
}
