package utils

import (
	"io/ioutil"
	"os"
	"os/exec"
	"net"
	"time"
	"strings"
	"github.com/coreos/go-log/log"
	"errors"
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
 * Function that validates an IPv4 and IPv6 address.
 *
 * @return bool
 */
func ValidIPAddress(ipAddress string) bool {
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
 * Note: This may break with FQDs
 */
func GetHostname() string {
	output, err := Execute("hostname")
	if err != nil {
		log.Error("Failed to obtain hostname.")
		os.Exit(1)
	}
	// Remove new line characters
	return strings.TrimSuffix(output, "\n")
}
/**
 * Function to return an IP and Port from a single ip:port string
 */
func splitIpPort(ipPort string) (string, string, error) {
	IPslice := strings.Split(ipPort, ":")

	if len(IPslice) < 2 {
		return "", "", errors.New("Invalid IP:Port string. Unable to split.")
	}

	return IPslice[0], IPslice[1], nil
}
