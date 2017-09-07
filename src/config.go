package main

import (
	"io/ioutil"
	"encoding/json"
	"os"
	"github.com/coreos/go-log/log"
)

type Config struct {
	Pulse Cluster `json:"pulse"`
	Pools Pools `json:"pools"`
	Nodes Nodes `json:"nodes"`
}

type Cluster struct {
	BindIP   string `json:"bind_address"`
	BindPort string `json:"bind_port"`
	TLS bool `json:"tls"`
}

type Pools struct {
	Pools map[string]Pool
}

type Pool struct {
	Nodes []string `json:"nodes"`
	FloatingIPs []string `json:"floating_ips"`
}

type Nodes struct {
	Nodes map[string]Node
}

type Node struct {
	IP   string `json:"ip"`
	Port string `json:"port"`
}

/**
 *
 */
func (c *Config) Load() {
	log.Debug("Loading configuration file")
	b, err := ioutil.ReadFile("./config.json")
	err = json.Unmarshal([]byte(b), c)

	if err != nil {
		log.Errorf("Unable to umarshal config: %s", err)
		os.Exit(1)
	}

	// We had an error attempting to decode the json into our struct! oops!
	if err != nil {
		log.Error("Unable to load config.json. Does it exist?")
		os.Exit(1)
	}
}

/**
 *
 */
func (c *Config) Save() {
	// Validate before we save
	c.Validate()
	// Convert struct back to JSON format
	configJSON, _ := json.MarshalIndent(c, "", "    ")
	// Save back to file
	err := ioutil.WriteFile("./config.json", configJSON, 0644)

	if err != nil {
		log.Error("Unable to save config.json. Does it exist?")
		os.Exit(1)
	}
}

/**
 *
 */
func (c *Config) Reload() {

}

/**
 *
 */
func (c *Config) Validate() {

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
