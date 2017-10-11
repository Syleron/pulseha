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
	"errors"
	"github.com/Syleron/PulseHA/proto"
	"github.com/coreos/go-log/log"
	"google.golang.org/grpc/connectivity"
	"sync"
	"time"
)

/**
Member struct type
*/
type Member struct {
	Hostname         string
	Status           proto.MemberStatus_Status
	Last_HC_Response time.Time
	Client
	sync.Mutex
}

/*
	Getters and setters for Member which allow us to make them go routine safe
*/

/**
  Set the last time this member received a health check
*/
func (m *Member) setLast_HC_Response(time time.Time) {
	m.Lock()
	defer m.Unlock()
	m.Last_HC_Response = time
}

/**
Get the last time this member received a health check
*/
func (m *Member) getLast_HC_Response() time.Time {
	m.Lock()
	defer m.Unlock()
	return m.Last_HC_Response
}

/**
Get member hostname
*/
func (m *Member) getHostname() string {
	m.Lock()
	defer m.Unlock()
	return m.Hostname
}

/**
Set member hostname
*/
func (m *Member) setHostname(hostname string) {
	m.Lock()
	defer m.Unlock()
	m.Hostname = hostname
}

/**
Get member status
*/
func (m *Member) getStatus() proto.MemberStatus_Status {
	m.Lock()
	defer m.Unlock()
	return m.Status
}

/**
Set member status
*/
func (m *Member) setStatus(status proto.MemberStatus_Status) {
	m.Lock()
	defer m.Unlock()
	m.Status = status
}

/**
Set member Client GRPC
*/
func (m *Member) setClient(client Client) {
	m.Client = client
}

/**
Note: Hostname is required for TLS as the certs are named after the hostname.
*/
func (m *Member) Connect() error {
	if (m.Connection == nil) || (m.Connection != nil && m.Connection.GetState() == connectivity.Shutdown) {
		log.Debug("creating new connection")
		nodeDetails, _ := NodeGetByName(m.Hostname)
		err := m.Client.Connect(nodeDetails.IP, nodeDetails.Port, m.Hostname)
		if err != nil {
			return err
		}
	}
	return nil
}

/**
Close the client connection
*/
func (m *Member) Close() {
	log.Debug("Member:Close() Connection closed")
	m.Client.Close()
}

/**
Send GRPC health check to current member
*/
func (m *Member) sendHealthCheck(data *proto.PulseHealthCheck) (interface{}, error) {
	if m.Connection == nil {
		return nil, errors.New("unable to send health check as member connection has not been initiated")
	}
	r, err := m.Send(SendHealthCheck, data)
	return r, err
}

/*
	Make the node active (bring up its groups)
*/
func (m *Member) makeActive() bool {
	log.Debugf("Member:makeActive() Making %s active", m.getHostname())

	if m.Hostname == gconf.getLocalNode() {
		makeMemberActive()
	} else {
		log.Debug("member is not local node making grpc call")
		_, err := m.Send(
			SendMakeActive,
			&proto.PulsePromote{
				Success: false,
				Message: "",
				Member:  m.getHostname(),
			},
		)
		if err != nil {
			log.Error(err)
			log.Errorf("Error making %s active. Error: %s", m.getHostname(), err.Error())
			return false
		}
	}
	m.Status = proto.MemberStatus_ACTIVE
	return true
}

/**
Make the node passive (take down its groups)
*/
func (m *Member) makePassive() bool {
	log.Debugf("Member:makePassive() Making %s passive", m.getHostname())
	if m.Hostname == gconf.getLocalNode() {
		makeMemberPassive()
	} else {
		log.Debug("member is not local node making grpc call")
		_, err := m.Send(
			SendMakePassive,
			&proto.PulsePromote{
				Success: false,
				Message: "",
				Member:  m.getHostname(),
			})
		if err != nil {
			log.Error(err)
			log.Errorf("Error making %s passive. Error: %s", m.getHostname(), err.Error())
			return false
		}
	}
	m.Status = proto.MemberStatus_PASSIVE
	return true
}

/**
Used to bring up a single IP on member
We need to know the group to work out what interface to
bring it up on.
*/
func (m *Member) bringUpIPs(ips []string, group string) bool {
	configCopy := gconf.GetConfig()
	iface := configCopy.GetGroupIface(m.Hostname, group)
	if m.Hostname == gconf.getLocalNode() {
		log.Debug("member is local node bringing up IP's")
		bringUpIPs(iface, ips)
	} else {
		log.Debug("member is not local node making grpc call")
		_, err := m.Send(
			SendBringUpIP,
			&proto.PulseBringIP{
				Iface: iface,
				Ips:   ips,
			})
		if err != nil {
			log.Error(err)
			log.Errorf("Error making %s passive. Error: %s", m.getHostname(), err.Error())
			return false
		}
	}
	m.Status = proto.MemberStatus_PASSIVE
	return true
}

/**
Monitor the last time we received a health check and or failover
*/
func (m *Member) monitorReceivedHCs() bool {
	elapsed := int64(time.Since(m.getLast_HC_Response())) / 1e9
	if int(elapsed) > 0 && int(elapsed)%4 == 0 {
		log.Warning("No health checks are being made.. Perhaps a failover is required?")
	}
	// If 30 seconds has gone by.. something is wrong.
	if int(elapsed) >= 30 {
		var addHCSuccess bool = false
		// TODO: Perform additional health checks plugin stuff HERE
		if !addHCSuccess {
			// Nothing has worked.. assume the master has failed. Fail over.
			hostname, _ := pulse.Server.Memberlist.getNextActiveMember()
			activeMember := pulse.Server.Memberlist.GetMemberByHostname(hostname)
			// set the current active appliance as unavailable
			activeMember.setStatus(proto.MemberStatus_UNAVAILABLE)
			if hostname == gconf.localNode {
				log.Info("Attempting a failover..")
				m.makeActive()
				return true
			}
			log.Info("Waiting on " + hostname + " to become active")
			m.setLast_HC_Response(time.Now())
			return false
		} else {
			m.setLast_HC_Response(time.Now())
		}
	}
	return false
}
