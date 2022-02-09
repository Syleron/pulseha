package main

import (
	"github.com/syleron/pulseha/packages/network"
	"github.com/syleron/pulseha/plugins/hcPing/packages/config"
	"github.com/syleron/pulseha/src/pulseha"
)

type PulseHCPing bool

const PluginName = "PingHC"
const PluginVersion = 1.0

const PluginWeight = 10

var (
	DB   *pulseha.Database
	conf config.Config
)

// TODO: What metrics and thresholds do we want to consider?
// TODO: Only works with IPv4 at the moment.

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
	_, err := db.Config.GetPluginConfig(e.Name())
	if err != nil {
		if err := db.Config.SetPluginConfig(e.Name(), conf.GenerateDefaultConfig()); err != nil {
			// TODO: Do something with this error
		}
	}
	// Define our config
	//conf = cfg.(config.Config)
	//fmt.Println(">>>^^^^^^^^^ ", conf.SmtpHost)

	// If not, create our config section in the main config w/ defaults
	// Change me
	return nil
}

func (e PulseHCPing) Send() error {
	//log.Info("sending ping to 127.0.0.1")
	if err := network.ICMPv4("12.0.0.1/24"); err != nil {
		// TODO: Do something when the ICMP check fails.
		//log.Info("ping failed", err)
		return err
	}
	//log.Info("ping passed")
	return nil
}

var PluginHC PulseHCPing
