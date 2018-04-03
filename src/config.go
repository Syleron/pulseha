/*
   PulseHA - HA Cluster Daemon
   Copyright (C) 2017-2018  Andrew Zak <andrew@pulseha.com>

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
	"github.com/Syleron/PulseHA/src/utils"
	log "github.com/Sirupsen/logrus"
	"io/ioutil"
	"os"
)

type Config struct {
	Pulse     Local               `json:"pulseha"`
	Groups    map[string][]string `json:"floating_ip_groups"`
	Nodes     map[string]Node     `json:"nodes"`
	Logging   Logging             `json:"logging"`
}

type Local struct {
	TLS bool `json:"tls"`
	HealthCheckInterval   int             `json:"hcs_interval"`
	FailOverInterval      int             `json:"fos_interval"`
	FailOverLimit         int             `json:"fo_limit"`
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
	Level     string `json:"level"`
	ToLogFile bool   `json:"to_logfile"`
	LogFile   string `json:"logfile"`
}

/**
 * Returns a copy of the config
 */
func (c *Config) GetConfig() Config {
	return *c
}

/**
 * Sets the local node name
 */
func (c *Config) setLocalNode() error {
	hostname := utils.GetHostname()
	log.Debugf("Config:setLocalNode Hostname is: %s", hostname)
	c.localNode = hostname
	return nil
}

/**

 */
func (c *Config) nodeCount() int {
	return len(c.Nodes)
}

/**
 * Return the local node name
 */
func (c *Config) getLocalNode() string {
	return c.localNode
}

/**
 * Function used to load the config
 */
func (c *Config) Load() {
	log.Info("Loading configuration file")
	b, err := ioutil.ReadFile("/etc/pulseha/config.json")
	if err != nil {
		log.Errorf("Error reading config file: %s", err)
		os.Exit(1)
	}
	err = json.Unmarshal([]byte(b), &c)
	gconf.Config = *c
	if err != nil {
		log.Errorf("Unable to unmarshal config: %s", err)
		os.Exit(1)
	}
	if err != nil {
		log.Error("Unable to load config.json. Does it exist?")
		os.Exit(1)
	}
	err = c.setLocalNode()
	if err != nil {
		log.Fatalf("The local Hostname does not match the configuration")
	}
}

/**
 * Function used to save the config
 */
func (c *Config) Save() {
	log.Debug("Saving config..")
	gconf.Lock()
	defer gconf.Unlock()
	// Validate before we save
	c.Validate()
	// Convert struct back to JSON format
	configJSON, _ := json.MarshalIndent(c, "", "    ")
	// Save back to file
	err := ioutil.WriteFile("/etc/pulseha/config.json", configJSON, 0644)
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
	var success bool = true

	// if we are in a cluster.. does our hostname exist?
	if c.ClusterCheck() {
		for name, _ := range c.Nodes {
			if _, ok := c.Nodes[name]; !ok {
				log.Error("Hostname mistmatch. Localhost does not exist in cluster config.")
				success = false
			}
		}
	}

	// TODO: Check if our hostname exists inthe cluster config
	// TODO: Check if we have valid network interface names


	// Handles if shit hits the roof
	if success == false {
		// log why we exited?
		os.Exit(0)
	}
}

/**
 *
 */
func (c *Config) LocalNode() Node {
	return c.Nodes[utils.GetHostname()]
}

/**
 * Private - Check to see if we are in a configured cluster or not.
 */
func (c *Config) ClusterCheck() bool {
	config := gconf.GetConfig()
	if len(config.Nodes) > 0 {
		return true
	}
	return false
}

/**
 * Return the total number of configured nodes we have in our config.
 */
func (c *Config) ClusterTotal() int {
	config := gconf.GetConfig()
	return len(config.Nodes)
}

/**
Returns the interface the group is assigned to
*/
func (c *Config) GetGroupIface(node string, groupName string) string {
	for nodeName, n := range c.Nodes {
		if nodeName == node {
			for iface, groups := range n.IPGroups {
				for _, group := range groups {
					if group == groupName {
						return iface
					}
				}
			}
		}
	}
	return ""
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
