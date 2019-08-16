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
	"encoding/json"
	"errors"
	log "github.com/Sirupsen/logrus"
	p "github.com/Syleron/PulseHA/proto"
	"github.com/Syleron/PulseHA/src/client"
	"github.com/Syleron/PulseHA/src/utils"
	"google.golang.org/grpc/connectivity"
	"sync"
	"time"
	)

/**
 * MemberList struct type
 */
type MemberList struct {
	Members []*Member
	sync.Mutex
}

/**

 */
func (m *MemberList) Lock() {
	m.Mutex.Lock()
}

/**

 */
func (m *MemberList) Unlock() {
	m.Mutex.Unlock()
}

/**
 * Add a member to the client list
 */
func (m *MemberList) AddMember(hostname string, client *client.Client) {
	if !m.MemberExists(hostname) {
		DB.Logging.Debug("MemberList:MemberAdd() " + hostname + " added to memberlist")
		m.Lock()
		newMember := &Member{}
		newMember.SetHostname(hostname)
		newMember.SetStatus(p.MemberStatus_UNAVAILABLE)
		newMember.SetClient(client)
		m.Members = append(m.Members, newMember)
		m.Unlock()
	} else {
		DB.Logging.Debug("MemberList:MemberAdd() Member " + hostname + " already exists. Skipping.")
	}
}

/**
 * Remove a member from the client list by hostname
 */
func (m *MemberList) MemberRemoveByName(hostname string) {
	DB.Logging.Debug("MemberList:MemberRemoveByName() " + hostname + " removed from the memberlist")
	m.Lock()
	defer m.Unlock()
	for i, member := range m.Members {
		if member.GetHostname() == hostname {
			m.Members = append(m.Members[:i], m.Members[i+1:]...)
		}
	}
}

/**
 * Return Member by hostname
 */
func (m *MemberList) GetMemberByHostname(hostname string) *Member {
	m.Lock()
	defer m.Unlock()
	if hostname == "" {
		DB.Logging.Warn("MemberList:GetMemberByHostname() Unable to get get member by hostname as hostname is empty!")
	}
	for _, member := range m.Members {
		if member.GetHostname() == hostname {
			return member
		}
	}
	return nil
}

/**
 * Return true/false whether a member exists or not.
 */
func (m *MemberList) MemberExists(hostname string) bool {
	m.Lock()
	defer m.Unlock()
	for _, member := range m.Members {
		if member.GetHostname() == hostname {
			return true
		}
	}
	return false
}

/**
 * Attempt to broadcast a client function to other nodes (clients) within the memberlist
 */
func (m *MemberList) Broadcast(funcName client.ProtoFunction, data interface{}) {
	DB.Logging.Debug("MemberList:Broadcast() Broadcasting " + funcName.String())
	m.Lock()
	defer m.Unlock()
	for _, member := range m.Members {
		// We don't want to broadcast to our self!
		hostname, err := utils.GetHostname()
		if err != nil {
			log.Error("cannot broadcast as unable to get local hostname")
			return
		}
		if member.GetHostname() == hostname {
			continue
		}
		DB.Logging.Debug("Broadcast: " + funcName.String() + " to member " + member.GetHostname())
		member.Connect()
		member.Send(funcName, data)
	}
}

/**
Setup process for the memberlist
*/
func (m *MemberList) Setup() {
	// Load members into our memberlist slice
	m.LoadMembers()
	// Check to see if we are in a cluster
	if DB.Config.ClusterCheck() {
		// Are we the only member in the cluster?
		if DB.Config.NodeCount() == 1 {
			// Disable start up delay
			DB.StartDelay = false
			// We are the only member in the cluster so
			// we are assume that we are now the active appliance.
			m.PromoteMember(DB.Config.GetLocalNode())
		} else {
			// come up passive and monitoring health checks
			localMember := m.GetMemberByHostname(DB.Config.GetLocalNode())
			localMember.SetLastHCResponse(time.Now())
			localMember.SetStatus(p.MemberStatus_PASSIVE)
			DB.Logging.Debug("MemberList:Setup() starting the monitor received health checks scheduler")
			go utils.Scheduler(
				localMember.MonitorReceivedHCs,
				time.Duration(DB.Config.Pulse.FailOverInterval) * time.Millisecond,
			)
		}
	}
}

/**
load the nodes in our config into our memberlist
*/
func (m *MemberList) LoadMembers() {
	for key := range DB.Config.Nodes {
		newClient := &client.Client{}
		m.AddMember(key, newClient)
	}
}

/**
Reload the memberlist
 */
func (m *MemberList) Reload() {
	DB.Logging.Debug("MemberList:ReloadMembers() Reloading member nodes")
	// Reload our config
	DB.Config.Reload()
	// clear local members
	m.LoadMembers()
}

/**
Get status of a specific member by hostname
*/
func (m *MemberList) MemberGetStatus(hostname string) (p.MemberStatus_Status, error) {
	m.Lock()
	defer m.Unlock()
	for _, member := range m.Members {
		if member.GetHostname() == hostname {
			return member.GetStatus(), nil
		}
	}
	return p.MemberStatus_UNAVAILABLE, errors.New("unable to find member with hostname " + hostname)
}

/*
	Return the hostname of the active member
	or empty string if non are active
*/
func (m *MemberList) GetActiveMember() (string, *Member) {
	for _, member := range m.Members {
		if member.GetStatus() == p.MemberStatus_ACTIVE {
			return member.GetHostname(), member
		}
	}
	return "", nil
}

/**
Promote a member within the memberlist to become the active
node
*/
func (m *MemberList) PromoteMember(hostname string) error {
	DB.Logging.Debug("MemberList:PromoteMember() MemberList promoting " + hostname + " as active member..")
	// Inform everyone in the cluster that a specific node is now the new active
	// Demote if old active is no longer active. promote if the passive is the new active.
	// get host is it active?
	// Make sure the hostname member exists
	member := m.GetMemberByHostname(hostname)
	if member == nil {
		DB.Logging.Warn("Unknown hostname " + hostname + " give in call to promoteMember")
		return errors.New("the specified host does not exist in the configured cluster")
	}
	// if unavailable check it works or do nothing?
	switch member.GetStatus() {
	case p.MemberStatus_UNAVAILABLE:
		//If we are the only node and just configured we will be unavailable
		if DB.Config.NodeCount() > 1 {
			DB.Logging.Warn("Unable to promote member " + member.GetHostname() + " because it is unavailable")
			return errors.New("unable to promote member as it is unavailable")
		}
	case p.MemberStatus_ACTIVE:
		DB.Logging.Warn("Unable to promote member " + member.GetHostname() + " as it is active")
		return errors.New("unable to promote member as it is already active")
	}
	// get the current active member
	_, activeMember := m.GetActiveMember()
	// handle if we do not have an active member
	if activeMember != nil {
		// Make the current Active appliance passive
		success := activeMember.MakePassive()
		if !success {
			DB.Logging.Warn("Failed to make " + activeMember.GetHostname() + " passive, continuing")
		}
		// TODO: Note: Do we need this?
		// Update our local value for the active member
		activeMember.SetStatus(p.MemberStatus_PASSIVE)
	}
	// make the hostname the new active
	success := member.MakeActive()
	// make new node active
	if !success {
		DB.Logging.Warn("Failed to promote " + member.GetHostname() + " to active. Falling back to " + activeMember.GetHostname())
		// Somethings gone wrong.. attempt to make the previous active - active again.
		success := activeMember.MakeActive()
		if !success {
			DB.Logging.Error("Failed to make reinstate the active node. Something is really wrong")
		}
		// Note: we don't need to update the active status as we should receive an updated memberlist from the active
	}
	return nil
}

/**
	Function is only to be run on the active appliance
	Note: THis is not the final function name.. or not sure if this is
          where this logic will stay.. just playing around at this point.
	monitors the connections states for each member
*/
func (m *MemberList) MonitorClientConns() bool {
	// make sure we are still the active appliance
	member, err := m.GetLocalMember()
	if err != nil {
		DB.Logging.Debug("MemberList:monitorClientConns() Client monitoring has stopped as it seems we are no longer in a cluster")
		return true
	}
	if member.GetStatus() == p.MemberStatus_PASSIVE {
		DB.Logging.Debug("MemberList:monitorClientConns() Client monitoring has stopped as we are no longer active")
		return true
	}
	for _, member := range m.Members {
		if member.GetHostname() == DB.Config.GetLocalNode() {
			continue
		}
		member.Connect()
		DB.Logging.Debug("MemberList:MonitorClientConns() " + member.Hostname + " connection status is " + member.Connection.GetState().String())
		switch member.Connection.GetState() {
		case connectivity.Idle:
		case connectivity.Ready:
			member.SetStatus(p.MemberStatus_PASSIVE)
		default:
			member.SetStatus(p.MemberStatus_UNAVAILABLE)
		}
	}
	return false
}

/**
Send health checks to users who have a healthy connection
*/
func (m *MemberList) AddHealthCheckHandler() bool {
	// make sure we are still the active appliance
	member, err := m.GetLocalMember()
	if err != nil {
		DB.Logging.Debug("MemberList:addHealthCheckhandler() Health check handler has stopped as it seems we are no longer in a cluster")
		return true
	}
	if member.GetStatus() == p.MemberStatus_PASSIVE {
		DB.Logging.Debug("MemberList:addHealthCheckHandler() Health check handler has stopped as it seems we are no longer active")
		return true
	}
	for _, member := range m.Members {
		if member.GetHostname() == DB.Config.GetLocalNode() {
			continue
		}
		if !member.GetHCBusy() && member.GetStatus() == p.MemberStatus_PASSIVE {
			memberlist := new(p.PulseHealthCheck)
			for _, member := range m.Members {
				newMember := &p.MemberlistMember{
					Hostname:     member.GetHostname(),
					Status:       member.GetStatus(),
					Latency:      member.GetLatency(),
					LastReceived: member.GetLastHCResponse().Format(time.RFC1123),
				}
				memberlist.Memberlist = append(memberlist.Memberlist, newMember)
			}
			go member.RoutineHC(memberlist)
		}
	}
	return false
}

/**
Sync local config with each member in the cluster.
*/
func (m *MemberList) SyncConfig() error {
	DB.Logging.Debug("MemberList:SyncConfig() Syncing config with peers..")
	// Return with our new updated config
	buf, err := json.Marshal(DB.Config.GetConfig())
	// Handle failure to marshal config
	if err != nil {
		return errors.New("unable to sync config " + err.Error())
	}
	m.Broadcast(client.SendConfigSync, &p.PulseConfigSync{
		Replicated: true,
		Config:     buf,
	})
	return nil
}

/**
Update the local memberlist statuses based on the proto memberlist message
*/
func (m *MemberList) Update(memberlist []*p.MemberlistMember) {
	DB.Logging.Debug("MemberList:update() Updating memberlist")
	m.Lock()
	defer m.Unlock()
	//do not update the memberlist if we are active
	for _, member := range memberlist {
		for _, localMember := range m.Members {
			if member.GetHostname() == localMember.GetHostname() {
				localMember.SetStatus(member.Status)
				localMember.SetLatency(member.Latency)
				// our local last received has priority
				if member.GetHostname() != DB.Config.GetLocalNode() {
					tym, _ := time.Parse(time.RFC1123, member.LastReceived)
					localMember.SetLastHCResponse(tym)
				}
				break
			}
		}
	}
}

/**
Calculate who's next to become active in the memberlist
*/
func (m *MemberList) GetNextActiveMember() (*Member, error) {
	for hostname, _ := range DB.Config.Nodes {
		member := m.GetMemberByHostname(hostname)
		if member == nil {
			panic("MemberList:getNextActiveMember() Cannot get member by hostname " + hostname)
		}
		if member.GetStatus() == p.MemberStatus_PASSIVE {
			log.Debug("MemberList:getNextActiveMember() " + member.GetHostname() + " is the new active appliance")
			return member, nil
		}
	}
	return &Member{}, errors.New("MemberList:getNextActiveMember() No new active member found")
}

/**

 */
func (m *MemberList) GetLocalMember() (*Member, error) {
	m.Lock()
	defer m.Unlock()
	for _, member := range m.Members {
		if member.GetHostname() == DB.Config.GetLocalNode() {
			return member, nil
		}
	}
	return &Member{}, errors.New("cannot get local member. Perhaps we are no longer in a cluster")
}

/**
Reset the memberlist when we are no longer in a cluster.
*/
func (m *MemberList) Reset() {
	m.Lock()
	defer m.Unlock()
	m.Members = []*Member{}
}
