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

package main

import (
	log "github.com/sirupsen/logrus"
	"github.com/syleron/pulseha/packages/network"
	"github.com/syleron/pulseha/packages/utils"
)

type PulseNetCore bool

const PluginName = "PulseHA-NetCore"
const PluginVersion = 1.0

// Name defines our plugin name
func (e PulseNetCore) Name() string {
	return PluginName
}

// Version defines our plugin version
func (e PulseNetCore) Version() float64 {
	return PluginVersion
}

// BringUpIPs is used to bring up floating IP addresses on fail over.
func (e PulseNetCore) BringUpIPs(iface string, ips []string) error {
	for _, ip := range ips {
		if err := network.BringIPup(iface, ip); err != nil {
			return err
		}
		if utils.IsIPv6(ip) {
			go network.IPv6NDP(iface)
		} else {
			go network.SendGARP(iface, ip)
		}
	}
	return nil
}

// BringDownIPs is used to bring down floating IP addresses on recovery, promotion, etc.
func (e PulseNetCore) BringDownIPs(iface string, ips []string) error {
	for _, ip := range ips {
		if err := network.BringIPdown(iface, ip); err != nil {
			log.Debug("failed to take down " + ip + " on interface " + iface + ". Perhaps it didn't exist on that interface?")
		}
	}
	return nil
}

var PluginNet PulseNetCore
