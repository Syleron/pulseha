package main

import (
	"github.com/syleron/pulse/client"
	"github.com/syleron/pulse/server"
	"sync"
	"os"
)

func main() {
	// Setup CLI
	if len(os.Args) > 1 {
		setupCLI()
	} else {
		//// Set logging
		//f, err := os.OpenFile("pulse.log", os.O_RDWR | os.O_CREATE | os.O_APPEND, 0666)
		//if err != nil {
		//	log.Error("Error opening file: %v", err)
		//	os.Exit(1)
		//}
		//defer f.Close()
		//// log to text file
		//log.SetOutput(io.MultiWriter(f, os.Stdout))
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
