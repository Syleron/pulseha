/*
   PulseHA - HA Cluster Daemon
   Copyright (C) 2017-2019  Andrew Zak <andrew@linux.com>

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
	"github.com/syleron/pulseha/proto"
	"github.com/syleron/pulseha/src/client"
	"github.com/syleron/pulseha/src/utils"
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
	*client.Client
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
func (m *Member) SetHCBusy(busy bool) {
	m.Lock()
	defer m.Unlock()
	m.HCBusy = busy
}

/**

 */
func (m *Member) GetHCBusy() bool {
	m.Lock()
	defer m.Unlock()
	return m.HCBusy
}

/**

 */
func (m *Member) SetLatency(latency string) {
	m.Lock()
	defer m.Unlock()
	m.Latency = latency
}

/**

 */
func (m *Member) GetLatency() string {
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
func (m *Member) GetLastHCResponse() time.Time {
	m.Lock()
	defer m.Unlock()
	return m.LastHCResponse
}

/**
Get member hostname
*/
func (m *Member) GetHostname() string {
	m.Lock()
	defer m.Unlock()
	return m.Hostname
}

/**
Set member hostname
*/
func (m *Member) SetHostname(hostname string) {
	m.Lock()
	defer m.Unlock()
	m.Hostname = hostname
}

/**
Get member status
*/
func (m *Member) GetStatus() proto.MemberStatus_Status {
	//log.Debug("Member:getStatus() called by " + MyCaller())
	m.Lock()
	defer m.Unlock()
	return m.Status
}

/**
Set member status
*/
func (m *Member) SetStatus(status proto.MemberStatus_Status) {
	DB.Logging.Debug("Member:setStatus() " + m.GetHostname() + " status set to " + status.String() + " called by " + MyCaller())
	m.Lock()
	defer m.Unlock()
	m.Status = status
}

/**
Set member Client GRPC
*/
func (m *Member) SetClient(client *client.Client) {
	m.Lock()
	defer m.Unlock()
	m.Client = client
}

/**
Note: Hostname is required for TLS as the certs are named after the hostname.
*/
func (m *Member) Connect() error {
	if (m.Connection == nil) || (m.Connection != nil && m.Connection.GetState() == connectivity.Shutdown) {
		_, nodeDetails, _ := nodeGetByHostname(m.Hostname)
		DB.Logging.Debug("Member:Connect() Attempting to connect with node " + m.Hostname + " " + nodeDetails.IP + ":" + nodeDetails.Port)
		err := m.Client.Connect(nodeDetails.IP, nodeDetails.Port, m.Hostname, true)
		if err != nil {
			log.Error("Member:Connect() " + err.Error())
			return err
		}
	}
	return nil
}

/**
Close the client connection
*/
func (m *Member) Close() {
	DB.Logging.Debug("Member:Close() Connection closed")
	m.Client.Close()
}

/**
Active function - Send GRPC health check to current member
*/
func (m *Member) SendHealthCheck(data *proto.PulseHealthCheck) (interface{}, error) {
	if m.Connection == nil {
		return nil, errors.New("unable to send health check as member connection has not been initiated")
	}
	startTime := time.Now()
	r, err := m.Send(client.SendHealthCheck, data)
	// This is a record for the active appliance to know when it was last sent/received!
	m.SetLastHCResponse(time.Now())
	elapsed := fmt.Sprint(time.Since(startTime).Round(time.Millisecond))
	m.SetLatency(elapsed)
	return r, err
}

/**
Send health check via a go routine and mark the HC busy/not
*/
func (m *Member) RoutineHC(data *proto.PulseHealthCheck) {
	m.SetHCBusy(true)
	_, err := m.SendHealthCheck(data)
	if err != nil {
		m.Close()
		m.SetStatus(proto.MemberStatus_UNAVAILABLE) // This may not be required
	}
	m.SetHCBusy(false)
}

/*
	Make the node active (bring up its groups)
*/
func (m *Member) MakeActive() bool {
	DB.Logging.Debug("Member:makeActive() Making " + m.GetHostname() + " active")
	localNode := DB.Config.GetLocalNode()
	// Make ourself active if we are referring to ourself
	if m.GetHostname() == localNode.Hostname {
		// Reset vars
		m.SetLatency("")
		m.SetLastHCResponse(time.Time{})
		// Set our state
		m.SetStatus(proto.MemberStatus_ACTIVE)
		// Bring up our addresses if we have any
		MakeLocalActive()
		// Start performing health checks
		DB.Logging.Debug("Member:PromoteMember() Starting client connections monitor")
		go utils.Scheduler(
			DB.MemberList.MonitorClientConns,
			time.Duration(DB.Config.Pulse.HealthCheckInterval)*time.Millisecond,
		)
		DB.Logging.Debug("Member:PromoteMember() Starting health check handler")
		go utils.Scheduler(
			DB.MemberList.AddHealthCheckHandler,
			time.Duration(DB.Config.Pulse.HealthCheckInterval)*time.Millisecond,
		)
	} else {
		// TODO: Handle the closing of this connection
		m.Connect()
		_, err := m.Send(
			client.SendPromote,
			&proto.PulsePromote{
				Member: m.GetHostname(),
			})
		// Handle if we have an error
		if err != nil {
			log.Error(err)
			log.Errorf("Error making %s active. Error: %s", m.GetHostname(), err.Error())
			return false
		}
	}
	return true
}

/**
Make the node passive (take down its groups)
*/
func (m *Member) MakePassive() error {
	DB.Logging.Debug("Member:makePassive() Making " + m.GetHostname() + " passive")
	localNode := DB.Config.GetLocalNode()
	if m.GetHostname() == localNode.Hostname {
		// do this regardless to make sure we dont have any groups up
		MakeLocalPassive()
		// Update member variables
		m.SetLastHCResponse(time.Now())
		// check if we are already passive before starting a new scheduler
		if m.GetStatus() != proto.MemberStatus_PASSIVE {
			m.SetStatus(proto.MemberStatus_PASSIVE)
			// Start the scheduler
			DB.Logging.Debug("Member:makePassive() Starting the monitor received health checks scheduler " + m.GetHostname())
			go utils.Scheduler(
				m.MonitorReceivedHCs,
				time.Duration(DB.Config.Pulse.FailOverInterval)*time.Millisecond,
			)
		}
	} else {
		// TODO: Handle the closing of this connection
		if err := m.Connect(); err != nil {
			log.Error(err)
			return err
		}
		_, err := m.Send(
			client.SendMakePassive,
			&proto.PulsePromote{
				Member: m.GetHostname(),
			})
		if err != nil {
			log.Errorf("Error making %s passive. Error: %s", m.GetHostname(), err.Error())
			return err
		}
	}
	return nil
}

/**
Used to bring up a single IP on member
Note: We need to know the group to work out what interface to
bring it up on.
*/
func (m *Member) BringUpIPs(ips []string, group string) bool {
	iface := DB.Config.GetGroupIface(m.Hostname, group)
	localNode := DB.Config.GetLocalNode()
	if m.Hostname == localNode.Hostname {
		DB.Logging.Debug("member is local node bringing up IP's")
		BringUpIPs(iface, ips)
	} else {
		DB.Logging.Debug("member is not local node making grpc call")
		_, err := m.Send(
			client.SendBringUpIP,
			&proto.PulseBringIP{
				Iface: iface,
				Ips:   ips,
			})
		if err != nil {
			log.Error(err)
			log.Errorf("Error making %s passive. Error: %s", m.GetHostname(), err.Error())
			return false
		}
	}
	return true
}

/**
Monitor the last time we received a health check and or failover
*/
func (m *Member) MonitorReceivedHCs() bool {
	// Clear routine
	if !DB.Config.ClusterCheck() {
		log.Debug("MonitorReceivedHCs() routine cleared")
		return true
	}
	// make sure we are still the active appliance
	member, err := DB.MemberList.GetLocalMember()
	if err != nil {
		DB.Logging.Debug("Member:monitorReceivedHCs() Health check received monitor disabled as we are no longer in a cluster")
		return true
	}
	if member.GetStatus() == proto.MemberStatus_ACTIVE {
		DB.Logging.Debug("Member:monitorReceivedHCs() Health check received monitor disabled as we are now active.")
		return true
	}
	// calculate elapsed time
	elapsed := math.Floor(float64(time.Since(m.GetLastHCResponse()).Seconds()))
	// determine if we might need to failover
	if int(elapsed) > 0 && int(elapsed)%4 == 0 {
		_, member := DB.MemberList.GetActiveMember()
		if member != nil {
			member.SetStatus(proto.MemberStatus_SUSPICIOUS)
		}
		DB.Logging.Debug("Member:MonitorReceivedHCs() No health checks are being made.. Perhaps a failover is required?")
	}
	// has our threshold been met? Failover?
	var foLimit int
	if DB.StartDelay && DB.StartInterval < 1 {
		foLimit = DB.Config.Pulse.FailOverLimit * 2
		DB.StartInterval++
	} else {
		foLimit = DB.Config.Pulse.FailOverLimit
		DB.StartDelay = false
	}
	if int(elapsed) >= (foLimit / 1000) {
		DB.Logging.Debug("Member:monitorReceivedHCs() Performing Failover..")
		var addHCSuccess bool = false
		// TODO: Perform additional health checks plugin stuff HERE
		if !addHCSuccess {
			// Nothing has worked.. assume the master has failed. Fail over.
			member, err := DB.MemberList.GetNextActiveMember()
			// no new active appliance was found
			if err != nil {
				DB.Logging.Warn("unable to find new active member.. we are now the active")
				// make ourself active as no new active can be found apparently
				m.MakeActive()
				return true
			}
			// If we are not the new member just return
			localNode := DB.Config.GetLocalNode()
			if member.GetHostname() != localNode.Hostname {
				DB.Logging.Info("Waiting on " + member.GetHostname() + " to become active")
				m.SetLastHCResponse(time.Now())
				return false
			}
			// get our current active member
			_, activeMember := DB.MemberList.GetActiveMember()
			// If we have an active appliance mark it unavailable
			if activeMember != nil {
				activeMember.SetStatus(proto.MemberStatus_UNAVAILABLE)
			}
			// lets go active
			member.MakeActive()
			// Set the FO priority
			member.SetLastHCResponse(time.Time{})
			DB.Logging.Info("Local node is now active")
			return true
		} else {
			m.SetLastHCResponse(time.Now())
		}
	}
	return false
}
