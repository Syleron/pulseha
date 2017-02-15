package main

import (
	"github.com/syleron/pulse/structures"
	"os"
	"encoding/json"
	"fmt"
)

/**
 * This function is to be used to load our JSON based config and decode it as a struct!
 */
func LoadConfig() structures.Configuration {
	file, _ := os.Open("config.json")
	decoder := json.NewDecoder(file)
	configuration := structures.Configuration{}
	err := decoder.Decode(&configuration)

	// We had an error attempting to decode the json into our struct! oops!
	if err != nil {
		fmt.Println("error:", err)

		return structures.Configuration{}
	}

	// Validate our config to ensure that there nothing obviously wrong.
	configuration.Validate()

	return configuration
}

/**
 * Execute system command.
 */
func Execute() error {
	return nil
}
