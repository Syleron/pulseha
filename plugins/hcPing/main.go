package main

import (
	"github.com/syleron/pulseha/packages/network"
)

type PulseHCPing bool

const PluginName = "PingHC"
const PluginVersion = 1.0

const PluginWeight = 10


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
