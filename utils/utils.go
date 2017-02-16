package utils

import (
	"github.com/syleron/pulse/structures"
	"os"
	"encoding/json"
	"fmt"
	"os/exec"
	"net"
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
func Execute(cmd string, args []string) (string, error){
	command := exec.Command(cmd, args...)

	//printCommand(command)
	output, err := command.CombinedOutput()

	if err != nil {
		//printError(err)
		return "", err
	}

	return string(output), err
}

/**
 * Important function to validate an IPv4 Addr.
 *
 * @return bool
 */
func validIPv4(ipAddress string) bool {
	testInput := net.ParseIP(ipAddress)

	if testInput.To4() == nil {
		return false
	}

	return true
}

/**
 * Important function to validate an IPv6 Addr.
 *
 * @return bool
 */
func validIPv6(ipAddress string) bool {
	testInput := net.ParseIP(ipAddress)

	if testInput.To16() == nil {
		return false
	}

	return true
}

