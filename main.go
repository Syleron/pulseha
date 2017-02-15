package main

import (
	"github.com/syleron/pulse/client"
	"github.com/syleron/pulse/server"
	"sync"
	"fmt"
)

func main() {
	fmt.Println("Pulse started..\nLoading Config..")
	// Setup wait group
	var wg sync.WaitGroup
	wg.Add(1)
	//Setup Server
	go server.Setup(&wg)
	// Server Client
	client.Setup()
	// Wait for waitgroup to finish
	wg.Wait()
}
