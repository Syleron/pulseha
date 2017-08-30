package main

import (
	"net"
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
 * Member node struct type
 */
type Member struct {
	Name   string
	Addr   net.IP
	Port uint16
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

	logger.Print("[INFO] Pulse: Initializing...")

	pulse := &Pulse{
		Server: &Server{
			Logger: logger,
		},
		Config: conf,
		Logger: logger,
	}

	// Setup background stuffs
	var wg sync.WaitGroup
	wg.Add(1)
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