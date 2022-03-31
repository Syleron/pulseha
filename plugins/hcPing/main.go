package main

import (
	"github.com/syleron/pulseha/packages/network"
	"github.com/syleron/pulseha/plugins/hcPing/packages/config"
	"github.com/syleron/pulseha/src/pulseha"
)

type PulseHCPing bool

const PluginName = "PingHC"
const PluginVersion = 1.0


var (
	DB   *pulseha.Database
    PluginWeight int64 = 10
	
	Threshold int64 = 1
	FailureCount int64 = 1
)

func (e PulseHCPing) Name() string {
	return PluginName
}

func (e PulseHCPing) Version() float64 {
	return PluginVersion
}

func (e PulseHCPing) Weight() int64 {
	return PluginWeight
}

func (e PulseHCPing) Run(db *pulseha.Database) error {
	// Set our database variable
	DB = db
	
	// Check to see if we have a plugin section
	conf, err := db.Config.GetPluginConfig(e.Name())
	
	
	// Write default section if one doesn't exist
	if err != nil {
		// Define config object
		c := config.Config{}
		if err := db.Config.SetPluginConfig(e.Name(), c.GenerateDefaultConfig()); err != nil {
			// TODO: Do something with this error
		}
		// We had to write a default config so the rest of Run will be skipped
		return nil
	}
	
	// Type our config section
	confC := conf.(map[string]interface{})
	
	// Set our custom config options
	PluginWeight = int64(confC["weight"].(float64))
	Threshold = int64(confC["threshold"].(float64))
	FailureCount = int64(confC["failureCount"].(float64))
	
	return nil
}

func (e PulseHCPing) Send() error {
	// Get our config section
	conf, err := DB.Config.GetPluginConfig(e.Name())
	
	// Handle any errors
	if err != nil {
		return nil
	}
	
	// Type our config section
	confC := conf.(map[string]interface{})
	
	// Iterate through our groups
	for _, group := range confC["groups"].([]interface{}) {
		g := group.(map[string]interface{})
		ips := g["ips"].([]interface{})
		// name := g["name"].(string)
		
		// Send our ICMP requests
		for _, ip := range ips {
			if err := network.ICMPv4(ip.(string)); err != nil {
				return err
			}
		} 
	}
	
	return nil
}

var PluginHC PulseHCPing
