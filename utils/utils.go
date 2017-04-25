package utils

import (
	"github.com/syleron/pulse/structures"
	"encoding/json"
	"os/exec"
	"net"
	"io/ioutil"
	"time"
)

/**
 * This function is to be used to load our JSON based config and decode it as a struct!
 */
func LoadConfig() structures.Configuration {
	c, _ := ioutil.ReadFile("./config.json")
	configuration := structures.Configuration{}
	json.Unmarshal([]byte(c), &configuration)

	// We had an error attempting to decode the json into our struct! oops!
	//if err != nil {
	//	fmt.Println("error:", err)
	//
	//	return structures.Configuration{}
	//}

	// Validate our config to ensure that there nothing obviously wrong.
	configuration.Validate()

	return configuration
}

/**
 * This function is used to save the struct back as json in the config.json file.
 */
func SaveConfig(config structures.Configuration) bool {
	// Validate before we save
	config.Validate()
	// Convert struct back to JSON format
	configJSON, _ := json.MarshalIndent(config, "", "    ")
	// Save back to file
	_ = ioutil.WriteFile("./config.json", configJSON, 0644)
	// Return successful
	return true
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
func ValidIPv4(ipAddress string) bool {
	testInput := net.ParseIP(ipAddress)

	if testInput.To4() == nil {
		return false
	}

	return true
}

/**
 * Function to schedule the execution every x time as time.Duration.
 */
func Scheduler(method func(), delay time.Duration) {
	for _ = range time.Tick(delay) {
		method()
	}
}
