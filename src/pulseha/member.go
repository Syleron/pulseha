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

// TODO: Create a method to update the score.

package pulseha

import (
	"errors"
	"fmt"
	log "github.com/sirupsen/logrus"
	"github.com/syleron/pulseha/packages/client"
	"github.com/syleron/pulseha/packages/utils"
	"github.com/syleron/pulseha/rpc"
	"google.golang.org/grpc/connectivity"
	"math"
	"sync"
	"time"
)

// Member defines our member object.
type Member struct {
	// The hostname of the repented node
	Hostname string
	// The status of the local member
	Status rpc.MemberStatus_Status
	// The last time a health check was received
	LastHCResponse time.Time
	// The latency between the active and the current passive member
	Latency string
	// Determines if the health check is being made.
	HCBusy bool
	// Used to determine which node to fail over to
	Score int
	// The client for the member that is used to send GRPC calls
	*client.Client
	// The mutex to lock the member object
	sync.Mutex
}

// Lock locks our member object.
func (m *Member) Lock() {
	//_, _, no, _ := runtime.Caller(1)
	//log.Debugf("Member:Lock() Lock set line: %d by %s", no, MyCaller())
	m.Mutex.Lock()
}

/**

 */

// Unlock unlocks our member object.
func (m *Member) Unlock() {
	//_, _, no, _ := runtime.Caller(1)
	//log.Debugf("Member:Unlock() Unlock set line: %d by %s", no, MyCaller())
	m.Mutex.Unlock()
}

// SetHCBusy locks to make the member routine safe.
func (m *Member) SetHCBusy(busy bool) {
	m.Lock()
	defer m.Unlock()
	m.HCBusy = busy
}

// GetHCBusy locks to make the member routine safe.
func (m *Member) GetHCBusy() bool {
	m.Lock()
	defer m.Unlock()
	return m.HCBusy
}

// SetLatency updates the latency for this member.
// Note: This is between this member and the active member.
func (m *Member) SetLatency(latency string) {
	m.Lock()
	defer m.Unlock()
	m.Latency = latency
}

// GetLatency returns the latency for this member.
// Note: This is between this member and the active member.
func (m *Member) GetLatency() string {
	m.Lock()
	defer m.Unlock()
	return m.Latency
}

// SetScore update the health check total score for a member.
func (m *Member) SetScore(score int) {
	m.Lock()
	defer m.Unlock()
	m.Score = score
}

// GetScore Return the health check score for a particular member
func (m *Member) GetScore() int {
	m.Lock()
	defer m.Unlock()
	return m.Score
}

// SetLastHCResponse updates the last time this member recieved a health check.
func (m *Member) SetLastHCResponse(time time.Time) {
	m.Lock()
	defer m.Unlock()
	m.LastHCResponse = time
}

// GetLastHCResponse returns the last time this member recieved a health check.
func (m *Member) GetLastHCResponse() time.Time {
	m.Lock()
	defer m.Unlock()
	return m.LastHCResponse
}

// GetHostname returns the hostname for a particular member.
func (m *Member) GetHostname() string {
	m.Lock()
	defer m.Unlock()
	return m.Hostname
}

// SetHostname defines the hostname for a particular member
func (m *Member) SetHostname(hostname string) {
	m.Lock()
	defer m.Unlock()
	m.Hostname = hostname
}

// GetStatus returns the status for a particular member.
func (m *Member) GetStatus() rpc.MemberStatus_Status {
	//log.Debug("Member:getStatus() called by " + MyCaller())
	m.Lock()
	defer m.Unlock()
	return m.Status
}

// SetStatus defines the status for a particular member
func (m *Member) SetStatus(status rpc.MemberStatus_Status) {
	DB.Logging.Debug("Member:setStatus() " + m.GetHostname() + " status set to " + status.String() + " called by " + MyCaller())
	m.Lock()
	defer m.Unlock()
	m.Status = status
	// Inform our plugin(s) of state change
	InformMLSChange()
}

// SetClient defines our client object for a member.
func (m *Member) SetClient(client *client.Client) {
	m.Lock()
	defer m.Unlock()
	m.Client = client
}

// Connect establish a connection with a particular member.
// Note: Member hostname is required for TLS reasons.
func (m *Member) Connect() error {
	if (m.Connection == nil) || (m.Connection != nil && m.Connection.GetState() == connectivity.Shutdown) {
		_, nodeDetails, _ := nodeGetByHostname(m.Hostname)
		DB.Logging.Debug("Member:Connect() Attempting to connect with node " + m.Hostname + " " + nodeDetails.IP + ":" + nodeDetails.Port)
		err := m.Client.Connect(nodeDetails.IP, nodeDetails.Port, true)
		if err != nil {
			log.Error("Member:Connect() " + err.Error())
			return err
		}
	}
	return nil
}

// Close terminates the client connection
func (m *Member) Close() {
	DB.Logging.Debug("Member:Close() Connection closed")
	m.Client.Close()
}

// SendHealthCheck sends GRPC health check to current member
// Type: Active node function
// Note: Consider sending this periodically instead of the base health check
func (m *Member) SendHealthCheck(data *rpc.HealthCheckRequest) (interface{}, error) {
	if m.Connection == nil {
		return rpc.HealthCheckResponse{}, errors.New("unable to send health check as member connection has not been initiated")
	}
	startTime := time.Now()
	r, err := m.Send(client.SendHealthCheck, data)
	// This is a record for the active appliance to know when it was last sent/received!
	m.SetLastHCResponse(time.Now())
	elapsed := fmt.Sprint(time.Since(startTime).Round(time.Millisecond))
	m.SetLatency(elapsed)
	return r, err
}

// RoutineHC used to periodically send RPC health check messages.
// @data *rpc.HealthCheckRequest The data we are sending with each HC
// Type: Routine function
func (m *Member) RoutineHC(data *rpc.HealthCheckRequest) {
	m.SetHCBusy(true)
	response, err := m.SendHealthCheck(data)
	if err != nil {
		m.Close()
		m.SetStatus(rpc.MemberStatus_UNAVAILABLE) // This may not be required
	}
	// Make sure we have a response
	if response != nil && m != nil {
		// Update our score
		m.SetScore(int(response.(*rpc.HealthCheckResponse).Score))
	}
	m.SetHCBusy(false)
}

// MakeActive promotes a particular member to become active.
func (m *Member) MakeActive() error {
	DB.Logging.Debug("Member:makeActive() Making " + m.GetHostname() + " active")

	// Inform our plugins
	for _, p := range DB.Plugins.GetGeneralPlugins() {
		go p.Plugin.(PluginGen).OnMemberFailover(*m)
	}

	// Get our local node object
	localNode, err := DB.Config.GetLocalNode()
	if err != nil {
		return errors.New("unable to retrieve local node configuration")
	}

	// Are we making ourself active?
	if m.GetHostname() == localNode.Hostname {
		// Reset vars
		m.SetLatency("")
		m.SetLastHCResponse(time.Time{})
		// Set our state
		m.SetStatus(rpc.MemberStatus_ACTIVE)
		// Bring up our addresses if we have any
		MakeLocalActive()
		// Start monitoring our member list
		DB.Logging.Debug("Member:PromoteMember() Starting client connections monitor")
		go utils.Scheduler(
			DB.MemberList.MonitorClientConns,
			time.Duration(DB.Config.Pulse.HealthCheckInterval)*time.Millisecond,
		)
		// Start performing health checks
		DB.Logging.Debug("Member:PromoteMember() Starting health check handler")
		go utils.Scheduler(
			DB.MemberList.AddHealthCheckHandler,
			time.Duration(DB.Config.Pulse.HealthCheckInterval)*time.Millisecond,
		)
		return nil
	}

	// Make sure we are connected to our member
	if err := m.Connect(); err != nil {
		log.Error(err.Error())
		return err
	}
	// Inform member to become active.
	_, err = m.Send(
		client.SendPromote,
		&rpc.PromoteRequest{
			Member: m.GetHostname(),
		})
	// Handle if we have an error
	if err != nil {
		log.Errorf("Error making %s active. Error: %s", m.GetHostname(), err.Error())
		return err
	}

	return nil
}

// MakePassive demotes a particular member to become passive.
func (m *Member) MakePassive() error {
	DB.Logging.Debug("Member:makePassive() Making " + m.GetHostname() + " passive")

	// Get our local node object
	localNode, err := DB.Config.GetLocalNode()
	if err != nil {
		return errors.New("unable to retrieve local node configuration")
	}

	// Are we making ourself passive?
	if m.GetHostname() == localNode.Hostname {
		// do this regardless to make sure we dont have any groups up
		MakeLocalPassive()
		// Update member variables
		m.SetLastHCResponse(time.Now())
		// check if we are already passive before starting a new scheduler
		if m.GetStatus() != rpc.MemberStatus_PASSIVE {
			m.SetStatus(rpc.MemberStatus_PASSIVE)
			// Start the scheduler
			DB.Logging.Debug("Member:makePassive() Starting the monitor received health checks scheduler " + m.GetHostname())
			go utils.Scheduler(
				m.MonitorReceivedHCs,
				time.Duration(DB.Config.Pulse.FailOverInterval)*time.Millisecond,
			)
		}
		return nil
	}

	// Make sure we are connected to our member
	if err := m.Connect(); err != nil {
		log.Error(err)
		return err
	}
	// Inform member to become passive.
	_, err = m.Send(
		client.SendMakePassive,
		&rpc.MakePassiveRequest{
			Member: m.GetHostname(),
		})
	// Handle if we have an error
	if err != nil {
		log.Errorf("Error making %s passive. Error: %s", m.GetHostname(), err.Error())
		return err
	}

	return nil
}

// BringUpIPs used to bring up a particular floating address on a member.
// Note: We need to know the group to work out what interface to
//       bring it up on.
// TODO: Return an error instead of a boolean
func (m *Member) BringUpIPs(ips []string, group string) bool {
	iface, err := DB.Config.GetGroupIface(m.Hostname, group)
	if err != nil {
		return false
	}
	localNode, err := DB.Config.GetLocalNode()
	if err != nil {
		DB.Logging.Error("BringUpIPs() unable to retrieve local node configuration")
		return false
	}
	if m.Hostname == localNode.Hostname {
		DB.Logging.Debug("member is local node bringing up IP's")
		BringUpIPs(iface, ips)
	} else {
		DB.Logging.Debug("member is not local node making grpc call")
		_, err := m.Send(
			client.SendBringUpIP,
			&rpc.UpIpRequest{
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

// MonitorReceivedHCs monitor the last time we received a health check and or fail over.
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
	if member.GetStatus() == rpc.MemberStatus_ACTIVE {
		DB.Logging.Debug("Member:monitorReceivedHCs() Health check received monitor disabled as we are now active.")
		return true
	}
	// calculate elapsed time
	elapsed := math.Floor(float64(time.Since(m.GetLastHCResponse()).Seconds()))
	// determine if we might need to failover
	if int(elapsed) > 0 && int(elapsed)%4 == 0 {
		_, member := DB.MemberList.GetActiveMember()
		if member != nil {
			member.SetStatus(rpc.MemberStatus_SUSPICIOUS)
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
		DB.Logging.Debug("Member:monitorReceivedHCs() Performing Fail-over..")
		// Nothing has worked.. assume the master has failed. Fail over.
		member, err := DB.MemberList.GetNextActiveMember()
		// no new active appliance was found
		if err != nil {
			DB.Logging.Warn("unable to find new active member.. we are now the active")
			// make ourselves active as no new active can be found apparently
			m.MakeActive()
			return true
		}
		// If we are not the new member just return
		localNode, err := DB.Config.GetLocalNode()
		if err != nil {
			DB.Logging.Error("unable to retrieve local node configuration")
			return false
		}
		if member.GetHostname() != localNode.Hostname {
			DB.Logging.Info("Waiting on " + member.GetHostname() + " to become active")
			m.SetLastHCResponse(time.Now())
			return false
		}
		// get our current active member
		_, activeMember := DB.MemberList.GetActiveMember()
		// If we have an active appliance mark it unavailable
		if activeMember != nil {
			activeMember.SetStatus(rpc.MemberStatus_UNAVAILABLE)
		}
		// lets go active
		member.MakeActive()
		// Set the FO priority
		member.SetLastHCResponse(time.Time{})
		DB.Logging.Info("Local node is now active")
		return true
	}
	return false
}
