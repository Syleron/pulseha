package main

import (
	"log"
	"os"
	"sync"
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
func Create(conf *Config) (*Pulse, error) {
	logger := conf.Logger

	if logger == nil {
		logOutput := conf.LogOutput

		if logOutput == nil {
			logOutput = os.Stderr
		}

		logger = log.New(logOutput, "", log.LstdFlags)
	}

	pulse := &Pulse{
		Server: &Server{
			Logger: logger,
		},
		Config: conf,
		Logger: logger,
	}

	// Load plugins
	_, err := LoadPlugins()

	if err != nil {
		logger.Printf("[ERR] Pulse: Failed to load plugins: %s", err)
		os.Exit(1)
	}

	// Setup background stuffs
	// Note: Perhaps look into not using a wait group
	var wg sync.WaitGroup
	wg.Add(1)
	go pulse.Client.Setup()
	go pulse.Server.Setup("0.0.0.0","8443")
	wg.Wait()

	return pulse, nil
}

/**
 * Essential Construct
 */
func main() {
	Create(DefaultLocalConfig())
}