package main

import (
	"io/ioutil"
	"encoding/json"
	"os"
	"github.com/coreos/go-log/log"
	"path/filepath"
)

type Config struct {
	Pulse Local `json:"pulse"`
	Groups map[string][]string `json:"floating_ip_groups"`
	Nodes map[string]Node `json:"nodes"`
	Logging Logging `json:"logging"`
}

type Local struct {
	TLS bool `json:"tls"`
}

type Nodes struct {
	Nodes map[string]Node
}

type Node struct {
	IP   string `json:"bind_address"`
	Port string `json:"bind_port"`
	IPGroups map[string][]string  `json:"group_assignments"`
}

type Logging struct {
	ToLogFile bool `json:"to_logfile"`
	LogFile string `json:"logfile"`
}

/**
 * Function used to load the config
 */
func (c *Config) Load() {
	log.Info("Loading configuration file")
	// Get project directory location
	dir, err := filepath.Abs(filepath.Dir(os.Args[0]))
	if err != nil {
		log.Emergency(err)
	}
	b, err := ioutil.ReadFile(dir + "/config.json")
	err = json.Unmarshal([]byte(b), c)

	if err != nil {
		log.Errorf("Unable to unmarshal config: %s", err)
		os.Exit(1)
	}

	// We had an error attempting to decode the json into our struct! oops!
	if err != nil {
		log.Error("Unable to load config.json. Does it exist?")
		os.Exit(1)
	}
}

/**
 * Function used to save the config
 */
func (c *Config) Save() {
	// Validate before we save
	c.Validate()
	// Convert struct back to JSON format
	configJSON, _ := json.MarshalIndent(c, "", "    ")
	// Get project directory location
	dir, err := filepath.Abs(filepath.Dir(os.Args[0]))
	if err != nil {
		log.Emergency(err)
	}
	// Save back to file
	err = ioutil.WriteFile(dir + "/config.json", configJSON, 0644)
	// Check for errors
	if err != nil {
		log.Error("Unable to save config.json. Does it exist?")
		os.Exit(1)
	}
}

/**
 * Reload the config file into memory.
 * Note: Need to clear memory value before calling Load()
 */
func (c *Config) Reload() {
	// Reload the config file
	c.Load()
}

/**
 *
 */
func (c *Config) Validate() {
}

func (c *Config) LocalNode() (Node) {
	return c.Nodes[GetHostname()]
}

/**
 *
 */
func DefaultLocalConfig() (*Config) {
	return &Config{
		//Cluster: {
		//	//ClusterName: GetHostname(),
		//	//BindIP: "0.0.0.0",
		//	//BindPort: "8443",
		//},
	}
}
