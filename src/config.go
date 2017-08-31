package main

import (
	"io/ioutil"
	"encoding/json"
	"os"
	"github.com/coreos/go-log/log"
)

type Config struct {
	Cluster Cluster `json:"cluster"`
	Pools Pools `json:"pools"`
	Nodes Nodes `json:"nodes"`
}

type Cluster struct {
	ClusterName string `json:"cluster_name"`
	BindIP   string `json:"bind_address"`
	BindPort string `json:"bind_port"`
}

type Pools struct {
	Pool map[string]Pool
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

func (c *Config) Save() {

}

func (c *Config) Reload() {

}

func (c *Config) Validate() {

}

func DefaultLocalConfig() (*Config) {
	return &Config{
		//Cluster: {
		//	//ClusterName: GetHostname(),
		//	//BindIP: "0.0.0.0",
		//	//BindPort: "8443",
		//},
	}
}
