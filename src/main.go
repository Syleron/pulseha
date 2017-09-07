package main

import (
	"github.com/coreos/go-log/log"
	"os"
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
	pulse := createPulse()

	// Load plugins
	_, err := LoadPlugins()

	if err != nil {
		log.Errorf("Failed to load plugins: %s", err)
		os.Exit(1)
	}

	// Setup go routine for client
	go pulse.Client.Setup()

	// Setup server
	pulse.Server.Setup(pulse.Config.Pulse.BindIP,pulse.Config.Pulse.BindPort)
}