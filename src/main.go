package main

import (
	"github.com/Syleron/Pulse/src/client"
	"github.com/Syleron/Pulse/src/server"
	"sync"
	"os"
)

func main() {
	// Setup CLI
	if len(os.Args) > 1 {
		setupCLI()
	} else {
		// Setup wait group
		var wg sync.WaitGroup
		wg.Add(1)
		// Setup Server
		go server.Setup()
		// Server Client
		go client.Setup()
		// Wait for wait group to finish
		wg.Wait()
	}
}
