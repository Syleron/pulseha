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
	Logger *log.Logger
}

/**
 * Create a new instance of PulseHA
 */
func Create() (*Pulse, error) {
	pulse := &Pulse{
		Server: &Server{},
		Config: &Config{},
	}

	// Load the config
	pulse.Config.Load()

	// Load plugins
	_, err := LoadPlugins()

	if err != nil {
		log.Errorf("Failed to load plugins: %s", err)
		os.Exit(1)
	}

	// Setup background stuffs
	go pulse.Client.Setup()
	pulse.Server.Setup(pulse.Config.Cluster.BindIP,pulse.Config.Cluster.BindPort)

	return pulse, nil
}

/**
 * Essential Construct
 */
func main() {
	Create()
}