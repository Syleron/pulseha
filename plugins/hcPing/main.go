package main

import "github.com/syleron/pulseha/packages/network"

type PulseHCPing bool

const PluginName = "PingHC"
const PluginVersion = 1.0

const PluginWeight = 0


// TODO: What metrics and thresholds do we want to consider?
// TODO: Only works with IPv4 at the moment.

func (e PulseHCPing) Name() string {
	return PluginName
}

func (e PulseHCPing) Version() float64 {
	return PluginVersion
}

func (e PulseHCPing) Send() (bool, bool) {
	if err := network.ICMPv4(""); err != nil {
		// TODO: Do something when the ICMP check fails.
	}
	return false, false
}

var PluginHC PulseHCPing
