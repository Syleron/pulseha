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
package server

import (
	"errors"
	"fmt"
	log "github.com/Sirupsen/logrus"
	"github.com/Syleron/PulseHA/proto"
	"github.com/Syleron/PulseHA/src/client"
	"github.com/Syleron/PulseHA/src/utils"
	"google.golang.org/grpc/connectivity"
	"math"
	"sync"
	"time"
)

/**
Member struct type
*/
type Member struct {
	// The hostname of the repented node
	Hostname string
	// The status of the local member
	Status proto.MemberStatus_Status
	// The last time a health check was received
	LastHCResponse time.Time
	// The latency between the active and the current passive member
	Latency string
	// Determines if the health check is being made.
	HCBusy bool
	// The client for the member that is used to send GRPC calls
	client.Client
	// The mutex to lock the member object
	sync.Mutex
}

/**

 */
func (m *Member) Lock() {
	//_, _, no, _ := runtime.Caller(1)
	//log.Debugf("Member:Lock() Lock set line: %d by %s", no, MyCaller())
	m.Mutex.Lock()
}

/**

 */
func (m *Member) Unlock() {
	//_, _, no, _ := runtime.Caller(1)
	//log.Debugf("Member:Unlock() Unlock set line: %d by %s", no, MyCaller())
	m.Mutex.Unlock()
}

/*
	Getters and setters for Member which allow us to make them go routine safe
*/

/**

 */
func (m *Member) setHCBusy(busy bool) {
	m.Lock()
	defer m.Unlock()
	m.HCBusy = busy
}

/**

 */
func (m *Member) getHCBusy() bool {
	m.Lock()
	defer m.Unlock()
	return m.HCBusy
}

/**

 */
func (m *Member) setLatency(latency string) {
	m.Lock()
	defer m.Unlock()
	m.Latency = latency
}

/**

 */
func (m *Member) getLatency() string {
	m.Lock()
	defer m.Unlock()
	return m.Latency
}

/**
  Set the last time this member received a health check
*/
func (m *Member) SetLastHCResponse(time time.Time) {
	m.Lock()
	defer m.Unlock()
	m.LastHCResponse = time
}

/**
Get the last time this member received a health check
*/
func (m *Member) getLastHCResponse() time.Time {
	m.Lock()
	defer m.Unlock()
	return m.LastHCResponse
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
	//log.Debug("Member:getStatus() called by " + MyCaller())
	m.Lock()
	defer m.Unlock()
	return m.Status
}

/**
Set member status
*/
func (m *Member) setStatus(status proto.MemberStatus_Status) {
	log.Debug("Member:setStatus() " + m.getHostname() + " status set to " + status.String() + " called by " + MyCaller())
	m.Lock()
	defer m.Unlock()
	m.Status = status
}

/**
Set member Client GRPC
*/
func (m *Member) setClient(client client.Client) {
	m.Client = client
}

/**
Note: Hostname is required for TLS as the certs are named after the hostname.
*/
func (m *Member) Connect() error {
	if (m.Connection == nil) || (m.Connection != nil && m.Connection.GetState() == connectivity.Shutdown) {
		nodeDetails, _ := nodeGetByName(m.Hostname)
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
Active function - Send GRPC health check to current member
*/
func (m *Member) sendHealthCheck(data *proto.PulseHealthCheck) (interface{}, error) {
	if m.Connection == nil {
		return nil, errors.New("unable to send health check as member connection has not been initiated")
	}
	startTime := time.Now()
	r, err := m.Send(client.SendHealthCheck, data)
	// This is a record for the active appliance to know when it was last sent/received!
	m.SetLastHCResponse(time.Now())
	elapsed := fmt.Sprint(time.Since(startTime).Round(time.Millisecond))
	m.setLatency(elapsed)
	return r, err
}

/**
Send health check via a go routine and mark the HC busy/not
*/
func (m *Member) routineHC(data *proto.PulseHealthCheck) {
	m.setHCBusy(true)
	_, err := m.sendHealthCheck(data)
	if err != nil {
		m.Close()
		m.setStatus(proto.MemberStatus_UNAVAILABLE) // This may not be required
	}
	m.setHCBusy(false)
}

/*
	Make the node active (bring up its groups)
*/
func (m *Member) makeActive() bool {
	log.Debugf("Member:makeActive() Making %s active", m.getHostname())
	// Make ourself active if we are refering to ourself
	if m.getHostname() == db.GetLocalNode() {
		MakeMemberActive()
		// Reset vars
		m.setLatency("")
		m.SetLastHCResponse(time.Time{})
		m.setStatus(proto.MemberStatus_ACTIVE)
		// Start performing health checks
		log.Debug("Member:PromoteMember() Starting client connections monitor")
		go utils.Scheduler(pulse.Server.Memberlist.monitorClientConns, 1*time.Second)
		log.Debug("Member:PromoteMember() Starting health check handler")
		go utils.Scheduler(pulse.Server.Memberlist.addHealthCheckHandler, 1*time.Second)
	} else {
		// TODO: Handle the closing of this connection
		m.Connect()
		_, err := m.Send(
			client.SendPromote,
			&proto.PulsePromote{
				Member: m.getHostname(),
			})
		// Handle if we have an error
		if err != nil {
			log.Error(err)
			log.Errorf("Error making %s passive. Error: %s", m.getHostname(), err.Error())
			return false
		}
	}
	return true
}

/**
Make the node passive (take down its groups)
*/
func (m *Member) makePassive() bool {
	log.Debugf("Member:makePassive() Making %s passive", m.getHostname())
	if m.getHostname() == db.GetLocalNode() {
		// do this regardless to make sure we dont have any groups up
		MakeMemberPassive()
		// Update member variables
		m.SetLastHCResponse(time.Now())
		// check if we are already passive before starting a new scheduler
		if m.getStatus() != proto.MemberStatus_PASSIVE {
			m.setStatus(proto.MemberStatus_PASSIVE)
			// Start the scheduler
			log.Debug("Member:makePassive() Starting the monitor received health checks scheduler " + m.getHostname())
			go utils.Scheduler(m.monitorReceivedHCs, 10000*time.Millisecond)
		}
	} else {
		// TODO: Handle the closing of this connection
		m.Connect()
		_, err := m.Send(
			client.SendMakePassive,
			&proto.PulsePromote{
				Member: m.getHostname(),
			})
		if err != nil {
			log.Error(err)
			log.Errorf("Error making %s passive. Error: %s", m.getHostname(), err.Error())
			return false
		}
	}
	return true
}

/**
Used to bring up a single IP on member
Note: We need to know the group to work out what interface to
bring it up on.
*/
func (m *Member) bringUpIPs(ips []string, group string) bool {
	configCopy := db.GetConfig()
	iface := configCopy.GetGroupIface(m.Hostname, group)
	if m.Hostname == db.GetLocalNode() {
		log.Debug("member is local node bringing up IP's")
		BringUpIPs(iface, ips)
	} else {
		log.Debug("member is not local node making grpc call")
		_, err := m.Send(
			client.SendBringUpIP,
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
	return true
}

/**
Monitor the last time we received a health check and or failover
*/
func (m *Member) monitorReceivedHCs() bool {
	// make sure we are still the active appliance
	member, err := pulse.getMemberlist().getLocalMember()
	if err != nil {
		log.Debug("Member:monitorReceivedHCs() Health check received monitor disabled as we are no longer in a cluster")
		return true
	}
	if member.getStatus() == proto.MemberStatus_ACTIVE {
		log.Debug("Member:monitorReceivedHCs() Health check received monitor disabled as we are now active.")
		return true
	}
	// calculate elapsed time
	elapsed := math.Floor(float64(time.Since(m.getLastHCResponse()).Seconds()))
	// determine if we might need to failover
	if int(elapsed) > 0 && int(elapsed)%4 == 0 {
		log.Warning("No health checks are being made.. Perhaps a failover is required?")
	}
	// has our threshold been met? Failover?
	//log.Info(elapsed)
	if int(elapsed) >= 10 {
		log.Debug("Member:monitorReceivedHCs() Performing Failover..")
		var addHCSuccess bool = false
		// TODO: Perform additional health checks plugin stuff HERE
		if !addHCSuccess {
			log.Warn("Additional health checks have failed.")
			// Nothing has worked.. assume the master has failed. Fail over.
			member, err := pulse.getMemberlist().getNextActiveMember()
			// no new active appliance was found
			if err != nil {
				log.Warn("unable to find new active member.. we are now the active")
				// make ourself active as no new active can be found apparently
				m.makeActive()
				return true
			}
			// If we are not the new member just return
			if member.getHostname() != db.GetLocalNode() {
				log.Info("Waiting on " + member.getHostname() + " to become active")
				m.SetLastHCResponse(time.Now())
				return false
			}
			// get our current active member
			_, activeMember := pulse.getMemberlist().getActiveMember()
			// If we have an active appliance mark it unavailable
			if activeMember != nil {
				activeMember.setStatus(proto.MemberStatus_UNAVAILABLE)
			}
			// lets go active
			member.makeActive()
			// Set the FO priority
			member.setLastHCResponse(time.Time{})
			log.Info("Local node is now active")
			return true
		} else {
			m.SetLastHCResponse(time.Now())
		}
	}
	return false
}
