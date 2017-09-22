/*
    PulseHA - HA Cluster Daemon
    Copyright (C) 2017  Andrew Zak <andrew@pulseha.com>

    This program is free software: you can redistribute it and/or modify
    it under the terms of the GNU Affero General Public License as published
    by the Free Software Foundation, either version 3 of the License, or
    (at your option) any later version.

    This program is distributed in the hope that it will be useful,
    but WITHOUT ANY WARRANTY; without even the implied warranty of
    MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
    GNU Affero General Public License for more details.

    You should have received a copy of the GNU Affero General Public License
    along with this program.  If not, see <http://www.gnu.org/licenses/>.
 */
package main

import (
	"encoding/json"
	"github.com/coreos/go-log/log"
	"io/ioutil"
	"os"
	"path/filepath"
)

type Config struct {
	Pulse   Local               `json:"pulse"`
	Groups  map[string][]string `json:"floating_ip_groups"`
	Nodes   map[string]Node     `json:"nodes"`
	Logging Logging             `json:"logging"`
}

type Local struct {
	TLS bool `json:"tls"`
}

type Nodes struct {
	Nodes map[string]Node
}

type Node struct {
	IP       string              `json:"bind_address"`
	Port     string              `json:"bind_port"`
	IPGroups map[string][]string `json:"group_assignments"`
}

type Logging struct {
	ToLogFile bool   `json:"to_logfile"`
	LogFile   string `json:"logfile"`
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
	err = ioutil.WriteFile(dir+"/config.json", configJSON, 0644)
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
	log.Debug("Reloading PulseHA config")
	// Reload the config file
	c.Load()
}

/**
 *
 */
func (c *Config) Validate() {
}

/**
 *
 */
func (c *Config) LocalNode() Node {
	return c.Nodes[GetHostname()]
}

/**
 *
 */
func DefaultLocalConfig() *Config {
	return &Config{
	//Cluster: {
	//	//ClusterName: GetHostname(),
	//	//BindIP: "0.0.0.0",
	//	//BindPort: "8443",
	//},
	}
}
