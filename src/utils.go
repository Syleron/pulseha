package main

import (
	"io/ioutil"
	"os"
	"os/exec"
	"net"
	"time"
	"github.com/coreos/go-log/log"
	"strings"
	"strconv"
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
func ValidIPAddress(ipAddress string) (bool) {
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
		log.Error("Failed to obtain hostname.")
		os.Exit(1)
	}
	// Remove new line characters
	return strings.TrimSuffix(output, "\n")
}

/**
 * Private - Check to see if we are in a configured cluster or not.
 */
func _clusterCheck(c *Config) (bool) {
	if len(c.Nodes) > 0 {
		return true
	}
	return false
}

/**
 *
 */
func _genGroupName(c *Config) (string) {
	totalGroups := len(c.Groups)
	for i := 1; i <= totalGroups; i++ {
		newName := "group" + strconv.Itoa(i)
		if _, ok := c.Groups[newName]; !ok {
			return newName
		}
	}
	return "group" + strconv.Itoa(totalGroups + 1)
}

func _groupExist(name string, c *Config) (bool) {
	if _, ok := c.Groups[name]; ok {
		return true
	}
	return false
}

/**
 *
 */
func _groupIPExist(name string, ip string, c *Config) (bool, int) {
	for index, cip := range c.Groups[name] {
		if ip == cip {
			return true, index
		}
	}
	return false, -1
}
