package utils

import (
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"net"
	"time"
)

/**
 * Load a specific file and return byte code
 **/
func LoadFile(file string) []byte {
	c, err := ioutil.ReadFile(file)

	// We had an error attempting to decode the json into our struct! oops!
	if err != nil {
		//log.Error("Unable to load file. Does it exist?")
		os.Exit(1)
	}

	return []byte(c)
}

/**
 * Execute system command.
 */
func Execute(cmd string, args ...string) (string, error) {
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

/**
 * Create folder if it doesn't already exist!
 * Returns true or false depending on whether the folder was created or not.
 */
func CreateFolder(path string) bool {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		os.Mkdir(path, os.ModePerm)
		return true
	}
	return false
}

/**
 * Check if a folder exists.
 */
func CheckFolderExist(path string) bool {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return false
	}
	return true
}

/**
 * Get local hostname
 */
func GetHostname() string {
	output, err := Execute("hostname", "-f")

	if err != nil {
		log.Fatal("Failed to obtain hostname.")
		os.Exit(1)
	}

	return output
}