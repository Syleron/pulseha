package main

import (
	"github.com/syleron/pulse/client"
	"github.com/syleron/pulse/server"
	"sync"
	log "github.com/Sirupsen/logrus"
	"os"
)

func main() {
	// Setup CLI
	if len(os.Args) > 1 {
		setupCLI()
	} else {
		log.Info("Pulse started..")
		// Setup wait group
		var wg sync.WaitGroup
		wg.Add(1)
		// Setup Server
		go server.Setup(&wg)
		// Server Client
		client.Setup()
		// Wait for wait group to finish
		wg.Wait()
	}
}
