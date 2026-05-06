// PulseHA - HA Cluster Daemon
// Copyright (C) 2017-2021  Andrew Zak <andrew@linux.com>
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published
// by the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <http://www.gnu.org/licenses/>.

package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"runtime"
	"sync"

	"github.com/google/uuid"

	log "github.com/charmbracelet/log"
	"github.com/syleron/pulseha/packages/jsonHelper"
	"github.com/syleron/pulseha/packages/utils"
)

var (
	CONFIG_DIR      = ""
	CONFIG_LOCATION = ""
	CLI_SOCKET_PATH = ""
)

func init() {
	// Try production directory first
	if os.Getenv("PULSEHA_DEV") != "true" {
		if err := os.MkdirAll("/etc/pulseha", 0755); err == nil {
			CONFIG_DIR = "/etc/pulseha"
			CONFIG_LOCATION = filepath.Join(CONFIG_DIR, "config.json")
			CLI_SOCKET_PATH = "/var/run/pulseha/pulseha.sock"
			return
		}
	}

	// Fall back to user's home directory
	homeDir, err := os.UserHomeDir()
	if err != nil {
		log.Fatal("Failed to get user home directory", "error", err)
	}

	CONFIG_DIR = filepath.Join(homeDir, ".pulseha")
	CONFIG_LOCATION = filepath.Join(CONFIG_DIR, "config.json")
	CLI_SOCKET_PATH = filepath.Join(CONFIG_DIR, "pulseha.sock")

	// Create user directory
	if err := os.MkdirAll(CONFIG_DIR, 0755); err != nil {
		log.Fatal("Failed to create config directory in home directory", "error", err)
	}

	log.Warn("Using user-local config directory", "path", CONFIG_DIR)
}

type Config struct {
	Pulse   Local                  `json:"pulseha"`
	Groups  map[string][]string    `json:"floating_ip_groups"`
	Nodes   map[string]*Node       `json:"nodes"`
	Plugins map[string]interface{} `json:"plugins"`
	sync.Mutex
}

type Local struct {
	HealthCheckInterval int    `json:"hcs_interval"`
	FailOverInterval    int    `json:"fos_interval"`
	FailOverLimit       int    `json:"fo_limit"`
	LocalNode           string `json:"local_node"`
	ClusterToken        string `json:"cluster_token"`
	LoggingLevel        string `json:"logging_level"`
	AutoFailback        bool   `json:"auto_failback"`
	LogToFile           bool   `json:"log_to_file"`
	LogFileLocation     string `json:"log_file_location"`
	LogToSyslog         bool   `json:"log_to_syslog"`   // Enable syslog logging
	SyslogNetwork       string `json:"syslog_network"`  // Network type: "", "tcp", "udp"
	SyslogAddress       string `json:"syslog_address"`  // Syslog server address
	SyslogFacility      string `json:"syslog_facility"` // Syslog facility: LOG_LOCAL0, etc.
	SyslogTag           string `json:"syslog_tag"`      // Syslog tag
	Mode                string `json:"mode"`            // active-passive or active-active
}

type Node struct {
	Hostname string              `json:"hostname"`
	IP       string              `json:"bind_address"`
	Port     string              `json:"bind_port"`
	IPGroups map[string][]string `json:"group_assignments"`
}

// New instantiates and setups up our config object
func New() (*Config, error) {
	// Create new config
	c := &Config{
		Pulse: Local{
			HealthCheckInterval: 1000,
			FailOverInterval:    5000,
			FailOverLimit:       10000,
			LoggingLevel:        "info",
			AutoFailback:        true,
			LogToFile:           false, // Default to syslog only
			LogFileLocation:     filepath.Join(CONFIG_DIR, "pulseha.log"),
			LogToSyslog:         true,
			SyslogNetwork:       "",
			SyslogAddress:       "",
			SyslogFacility:      "LOG_INFO",
			SyslogTag:           "pulseha",
			Mode:                "active-passive",
		},
		Groups:  make(map[string][]string),
		Nodes:   make(map[string]*Node),
		Plugins: make(map[string]interface{}),
	}

	// Load the config
	if err := c.Load(); err != nil {
		return nil, err
	}

	// Ensure maps are initialized
	if c.Groups == nil {
		c.Groups = make(map[string][]string)
	}
	if c.Nodes == nil {
		c.Nodes = make(map[string]*Node)
	}
	if c.Plugins == nil {
		c.Plugins = make(map[string]interface{})
	}

	// Ensure no null values in groups
	for groupName, ips := range c.Groups {
		if ips == nil {
			c.Groups[groupName] = make([]string, 0)
		}
	}

	return c, nil
}

// GetConfig - Returns a pointer to the config (not a copy to avoid copying mutex)
func (c *Config) GetConfig() *Config {
	return c
}

// NodeCount - Returns the total number of nodes in the configured cluster
func (c *Config) NodeCount() int {
	return len(c.Nodes)
}

// GetLocalNodeUUID returns the UUID of the local node
func (c *Config) GetLocalNodeUUID() (string, error) {
	if !c.ClusterCheck() {
		return "", errors.New("cluster check failed")
	}
	return c.Pulse.LocalNode, nil
}

// GetLocalNodeForBinding gets local node config for initial server binding, bypassing cluster check
func (c *Config) GetLocalNodeForBinding() (Node, error) {
	if c.Pulse.LocalNode == "" {
		return Node{}, errors.New("no local node specified")
	}
	if node, ok := c.Nodes[c.Pulse.LocalNode]; ok {
		// Create a deep copy of the node
		nodeCopy := Node{
			Hostname: node.Hostname,
			IP:       node.IP,
			Port:     node.Port,
		}
		// Deep copy the IPGroups map
		if node.IPGroups != nil {
			nodeCopy.IPGroups = make(map[string][]string)
			for k, v := range node.IPGroups {
				nodeCopy.IPGroups[k] = make([]string, len(v))
				copy(nodeCopy.IPGroups[k], v)
			}
		}
		return nodeCopy, nil
	}
	return Node{}, fmt.Errorf("local node '%s' not found in configuration", c.Pulse.LocalNode)
}

// GetLocalNode attempt to get local node definition in our config.
func (c *Config) GetLocalNode() (Node, error) {
	if !c.ClusterCheck() {
		return Node{}, errors.New("cluster check failed")
	}
	uuid, err := c.GetLocalNodeUUID()
	if err != nil {
		return Node{}, err
	}
	if node, ok := c.Nodes[uuid]; ok {
		// Create a deep copy of the node
		nodeCopy := Node{
			Hostname: node.Hostname,
			IP:       node.IP,
			Port:     node.Port,
		}
		// Deep copy the IPGroups map
		if node.IPGroups != nil {
			nodeCopy.IPGroups = make(map[string][]string)
			for k, v := range node.IPGroups {
				// Create a new slice for each group
				groupCopy := make([]string, len(v))
				copy(groupCopy, v)
				nodeCopy.IPGroups[k] = groupCopy
			}
		}
		return nodeCopy, nil
	}
	return Node{}, errors.New("local node not found in config")
}

func MyCaller() string {
	// we get the callers as uintptrs - but we just need 1
	fpcs := make([]uintptr, 1)
	// skip 3 levels to get to the caller of whoever called Caller()
	n := runtime.Callers(3, fpcs)
	if n == 0 {
		return "n/a" // proper error her would be better
	}
	// get the info of the actual function that's in the pointer
	fun := runtime.FuncForPC(fpcs[0] - 1)
	if fun == nil {
		return "n/a"
	}
	// return its name
	return fun.Name()
}

// Load - Used to load the config into memory
func (c *Config) Load() error {
	c.Lock()
	defer c.Unlock()

	// Initialize maps if nil
	if c.Groups == nil {
		c.Groups = make(map[string][]string)
	}
	if c.Nodes == nil {
		c.Nodes = make(map[string]*Node)
	}
	if c.Plugins == nil {
		c.Plugins = make(map[string]interface{})
	}

	// Skip loading from disk in test mode
	if os.Getenv("PULSEHA_TEST") == "true" {
		log.Debug("Test mode: skipping loading config from disk")
		return nil
	}

	// Check if config exists
	if utils.CheckFileExists(CONFIG_LOCATION) {
		b, err := ioutil.ReadFile(CONFIG_LOCATION)
		if err != nil {
			return fmt.Errorf("error reading config file: %v", err)
		}
		if err = json.Unmarshal(b, &c); err != nil {
			return fmt.Errorf("unable to unmarshal config: %v", err)
		}

		// Migrate old configs: set default syslog values if missing
		c.migrateConfig()

		if err := c.Validate(); err != nil {
			return fmt.Errorf("config validation failed: %v", err)
		}
	} else {
		// Create a default config
		if err := c.SaveDefaultLocalConfig(); err != nil {
			return fmt.Errorf("unable to create default config: %v", err)
		}
	}
	return nil
}

// migrateConfig ensures backward compatibility by setting default values for new fields
func (c *Config) migrateConfig() {
	migrated := false

	// Check if syslog fields are missing and set defaults
	if c.Pulse.SyslogNetwork == "" && c.Pulse.SyslogAddress == "" &&
		c.Pulse.SyslogFacility == "" && c.Pulse.SyslogTag == "" {
		// This looks like an old config, set syslog defaults
		c.Pulse.LogToSyslog = true
		c.Pulse.SyslogNetwork = ""
		c.Pulse.SyslogAddress = ""
		c.Pulse.SyslogFacility = "LOG_INFO"
		c.Pulse.SyslogTag = "pulseha"
		migrated = true
		log.Debug("Migrated config: added default syslog settings")
	}

	// Save the migrated config if changes were made
	if migrated {
		if err := c.Save(); err != nil {
			log.Warn("Failed to save migrated config", "error", err)
		} else {
			log.Info("Config migrated successfully with new syslog settings")
		}
	}
}

// Reload the config file into memory.
func (c *Config) Reload() error {
	log.Info("Reloading PulseHA config", "component", "config")
	return c.Load()
}

// SaveDefaultLocalConfig - Generate a default config to write
func (c *Config) SaveDefaultLocalConfig() error {
	hostname, _ := os.Hostname()
	defaultConfig := &Config{
		Pulse: Local{
			HealthCheckInterval: 1000,
			FailOverInterval:    5000,
			FailOverLimit:       10000,
			AutoFailback:        true,
			LocalNode:           hostname,
			ClusterToken:        "",
			LoggingLevel:        "info",
			LogToFile:           false, // Default to syslog only
			LogFileLocation:     filepath.Join(CONFIG_DIR, "pulseha.log"),
			LogToSyslog:         true,
			SyslogNetwork:       "",
			SyslogAddress:       "",
			SyslogFacility:      "LOG_INFO",
			SyslogTag:           "pulseha",
			Mode:                "active-passive",
		},
		Groups:  make(map[string][]string),
		Nodes:   make(map[string]*Node),
		Plugins: make(map[string]interface{}),
	}

	// Set our config in memory
	c.Pulse = defaultConfig.Pulse
	c.Groups = defaultConfig.Groups
	c.Nodes = defaultConfig.Nodes
	c.Plugins = defaultConfig.Plugins

	// Skip writing to disk in test mode
	if os.Getenv("PULSEHA_TEST") == "true" {
		log.Debug("Test mode: skipping writing default config to disk")
		return nil
	}

	// Convert struct back to JSON format
	configJSON, err := json.MarshalIndent(c, "", "    ")
	if err != nil {
		return fmt.Errorf("failed to marshal config: %v", err)
	}

	// Create config directory if it doesn't exist
	if err := os.MkdirAll(filepath.Dir(CONFIG_LOCATION), 0755); err != nil {
		return fmt.Errorf("failed to create config directory: %v", err)
	}

	// Save back to file
	if err := ioutil.WriteFile(CONFIG_LOCATION, configJSON, 0600); err != nil {
		return fmt.Errorf("failed to write config file: %v", err)
	}

	return nil
}

// Validate validates the config
func (c *Config) Validate() error {
	// Validate mode
	if c.Pulse.Mode != "" && c.Pulse.Mode != "active-passive" && c.Pulse.Mode != "active-active" {
		return fmt.Errorf("invalid mode %q: must be either 'active-passive' or 'active-active'", c.Pulse.Mode)
	}

	// Skip hostname validation if we're in test mode
	if os.Getenv("PULSEHA_TEST") == "true" {
		return nil
	}

	if c.Groups == nil {
		c.Groups = make(map[string][]string)
	}

	if c.Nodes == nil {
		c.Nodes = make(map[string]*Node)
	}

	if c.Plugins == nil {
		c.Plugins = make(map[string]interface{})
	}

	// Only validate cluster configuration if we're in a cluster
	if c.ClusterCheck() {
		// Check if we have a local node UUID set
		if c.Pulse.LocalNode == "" {
			return errors.New("local node UUID is not set")
		}

		// Verify that node exists in our config
		if _, ok := c.Nodes[c.Pulse.LocalNode]; !ok {
			return errors.New("local node UUID does not exist in nodes section")
		}
	}

	// Log interval values for debugging
	log.Debug("Validating intervals", "healthcheck", c.Pulse.HealthCheckInterval, "failover", c.Pulse.FailOverInterval, "failover_limit", c.Pulse.FailOverLimit)

	if c.Pulse.HealthCheckInterval < 1000 {
		return fmt.Errorf("health check interval must be at least 1000ms (got %d)", c.Pulse.HealthCheckInterval)
	}
	if c.Pulse.FailOverInterval < 1000 {
		return fmt.Errorf("failover interval must be at least 1000ms (got %d)", c.Pulse.FailOverInterval)
	}
	if c.Pulse.FailOverLimit < 1000 {
		return fmt.Errorf("failover limit must be at least 1000ms (got %d)", c.Pulse.FailOverLimit)
	}

	if c.Pulse.FailOverLimit < c.Pulse.FailOverInterval {
		return errors.New("failover interval must be smaller than failover limit")
	}

	return nil
}

// LocalNode - Get the local node object
func (c *Config) LocalNode() Node {
	hostname, err := utils.GetHostname()
	if err != nil {
		return Node{}
	}
	_, node, err := c.GetNodeByHostname(hostname)
	if err != nil {
		return Node{}
	}
	// Create a copy of the node to return
	return Node{
		Hostname: node.Hostname,
		IP:       node.IP,
		Port:     node.Port,
		IPGroups: node.IPGroups,
	}
}

// ClusterCheck - Check to see if wea re in a configured cluster or not.
func (c *Config) ClusterCheck() bool {
	return len(c.Nodes) > 0
}

/*
*
Returns the interface the group is assigned to
*/
func (c *Config) GetGroupIface(hostname string, groupName string) (ifaceName string, err error) {
	for _, n := range c.Nodes {
		if n.Hostname == hostname {
			for iface, groups := range n.IPGroups {
				for _, group := range groups {
					if group == groupName {
						return iface, nil
					}
				}
			}
		}
	}
	return "", errors.New("cannot find interface assignment for group")
}

/*
*
Returns the hostname for a node based of it's IP address
*/
func (c *Config) GetNodeHostnameByAddress(address string) (string, error) {
	for _, node := range c.Nodes {
		if node.IP == address {
			return node.Hostname, nil
		}
	}
	return "", errors.New("unable to find node with IP address " + address)
}

// GetNodeByHostname returns the UUID and Node for a given hostname
func (c *Config) GetNodeByHostname(hostname string) (string, *Node, error) {
	for uuid, node := range c.Nodes {
		if node.Hostname == hostname {
			return uuid, node, nil
		}
	}
	return "", nil, fmt.Errorf("unable to find node with hostname %s", hostname)
}

// GenerateNodeID generates a new UUID for a node
func (c *Config) GenerateNodeID() string {
	return uuid.New().String()
}

// UpdateValue - Update a key's value
func (c *Config) UpdateValue(key string, value string) error {
	if err := jsonHelper.SetStructFieldByTag(key, value, &c.Pulse); err != nil {
		return err
	}
	if err := c.Validate(); err != nil {
		return errors.New("invalid configuration value")
	}
	// Save our config with the updated info
	if err := c.Save(); err != nil {
		return err
	}
	return nil
}

// UpdateHostname - Changes our local node hostname and the hostname in our node section
func (c *Config) UpdateHostname(newHostname string) error {
	uuid, err := c.GetLocalNodeUUID()
	if err != nil {
		return fmt.Errorf("failed to get local node UUID: %v", err)
	}
	if node, ok := c.Nodes[uuid]; ok {
		node.Hostname = newHostname
		return nil
	}
	return errors.New("local node not found in config")
}

func (c *Config) GetPluginConfig(pName string) (interface{}, error) {
	log.Debug("Getting plugin config", "plugin", pName)
	pluginConfig := c.Plugins[pName]
	if pluginConfig != nil {
		return c.Plugins[pName], nil
	}
	return nil, errors.New("plugin does not exist in config")
}

func (c *Config) SetPluginConfig(pName string, data interface{}) error {
	log.Debug("Setting plugin config", "plugin", pName)
	_, err := c.GetPluginConfig(pName)
	if err != nil {
		c.Lock()
		c.Plugins[pName] = data
		c.Unlock()
	}
	c.Save()
	return nil
}

// Save writes the config to disk
func (c *Config) Save() error {
	c.Lock()
	defer c.Unlock()

	// Ensure no null values in groups
	for groupName, ips := range c.Groups {
		if ips == nil {
			c.Groups[groupName] = make([]string, 0)
		}
	}

	if err := c.Validate(); err != nil {
		return fmt.Errorf("validation failed: %v", err)
	}

	// Always write to disk (tests may rely on persisted config)
	data, err := json.MarshalIndent(c, "", "    ")
	if err != nil {
		return fmt.Errorf("failed to marshal config: %v", err)
	}

	if err := ioutil.WriteFile(CONFIG_LOCATION, data, 0600); err != nil {
		return fmt.Errorf("failed to write config: %v", err)
	}

	return nil
}
