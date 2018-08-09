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
	"encoding/json"
	"errors"
	p "github.com/Syleron/PulseHA/proto"
	log "github.com/Sirupsen/logrus"
	"google.golang.org/grpc/connectivity"
	"sync"
	"time"
)

/**
 * Memberlist struct type
 */
type Memberlist struct {
	Members []*Member
	sync.Mutex
}

/**

 */
func (m *Memberlist) Lock() {
	m.Mutex.Lock()
}

/**

 */
func (m *Memberlist) Unlock() {
	m.Mutex.Unlock()
}

/**
 * Add a member to the client list
 */
func (m *Memberlist) AddMember(hostname string, client *Client) {
	if !m.MemberExists(hostname) {
		log.Debug("Memberlist:MemberAdd() " + hostname + " added to memberlist")
		m.Lock()
		newMember := &Member{}
		newMember.setHostname(hostname)
		newMember.setStatus(p.MemberStatus_UNAVAILABLE)
		newMember.setClient(*client)
		m.Members = append(m.Members, newMember)
		m.Unlock()
	} else {
		log.Debug("Memberlist:MemberAdd() Member " + hostname + " already exists. Skipping.")
	}
}

/**
 * Remove a member from the client list by hostname
 */
func (m *Memberlist) MemberRemoveByName(hostname string) {
	log.Debug("Memberlist:MemberRemoveByName() " + hostname + " removed from the memberlist")
	m.Lock()
	defer m.Unlock()
	for i, member := range m.Members {
		if member.getHostname() == hostname {
			m.Members = append(m.Members[:i], m.Members[i+1:]...)
		}
	}
}

/**
 * Return Member by hostname
 */
func (m *Memberlist) GetMemberByHostname(hostname string) *Member {
	m.Lock()
	defer m.Unlock()
	if hostname == "" {
		log.Warning("Memberlist:GetMemberByHostname() Unable to get get member by hostname as hostname is empty!")
	}
	for _, member := range m.Members {
		if member.getHostname() == hostname {
			return member
		}
	}
	return nil
}

/**
 * Return true/false whether a member exists or not.
 */
func (m *Memberlist) MemberExists(hostname string) bool {
	m.Lock()
	defer m.Unlock()
	for _, member := range m.Members {
		if member.getHostname() == hostname {
			return true
		}
	}
	return false
}

/**
 * Attempt to broadcast a client function to other nodes (clients) within the memberlist
 */
func (m *Memberlist) Broadcast(funcName protoFunction, data interface{}) {
	log.Debug("Memberlist:Broadcast() Broadcasting " + funcName.String())
	m.Lock()
	defer m.Unlock()
	for _, member := range m.Members {
		// We don't want to broadcast to our self!
		hostname, err := GetHostname()
		if err != nil {
			log.Error("cannot broadcast as unable to get local hostname")
			return
		}
		if member.getHostname() == hostname {
			continue
		}
		log.Debugf("Broadcast: %s to member %s", funcName.String(), member.getHostname())
		member.Connect()
		member.Send(funcName, data)
	}
}

/**
Setup process for the memberlist
*/
func (m *Memberlist) Setup() {
	// Load members into our memberlist slice
	m.LoadMembers()
	// Check to see if we are in a cluster
	if gconf.clusterCheck() {
		// Are we the only member in the cluster?
		if gconf.clusterTotal() == 1 {
			// We are the only member in the cluster so
			// we are assume that we are now the active appliance.
			m.PromoteMember(gconf.getLocalNode())
		} else {
			// come up passive and monitoring health checks
			localMember := m.GetMemberByHostname(gconf.getLocalNode())
			localMember.setLastHCResponse(time.Now())
			localMember.setStatus(p.MemberStatus_PASSIVE)
			log.Debug("Memberlist:Setup() - starting the monitor received health checks scheduler")
			go Scheduler(localMember.monitorReceivedHCs, 2000*time.Millisecond)
		}
	}
}

/**
load the nodes in our config into our memberlist
*/
func (m *Memberlist) LoadMembers() {
	config := gconf.GetConfig()
	for key := range config.Nodes {
		newClient := &Client{}
		m.AddMember(key, newClient)
	}
}

/**

 */
func (m *Memberlist) Reload() {
	log.Debug("Memberlist:ReloadMembers() Reloading member nodes")
	// Do a config reload
	gconf.reload()
	// clear local members
	m.LoadMembers()
}

/**
Get status of a specific member by hostname
*/
func (m *Memberlist) MemberGetStatus(hostname string) (p.MemberStatus_Status, error) {
	m.Lock()
	defer m.Unlock()
	for _, member := range m.Members {
		if member.getHostname() == hostname {
			return member.getStatus(), nil
		}
	}
	return p.MemberStatus_UNAVAILABLE, errors.New("unable to find member with hostname " + hostname)
}

/*
	Return the hostname of the active member
	or empty string if non are active
*/
func (m *Memberlist) getActiveMember() (string, *Member) {
	for _, member := range m.Members {
		if member.getStatus() == p.MemberStatus_ACTIVE {
			return member.getHostname(), member
		}
	}
	return "", nil
}

/**
Promote a member within the memberlist to become the active
node
*/
func (m *Memberlist) PromoteMember(hostname string) error {
	log.Debug("Memberlist:PromoteMember() Memberlist promoting " + hostname + " as active member..")
	// Inform everyone in the cluster that a specific node is now the new active
	// Demote if old active is no longer active. promote if the passive is the new active.
	// get host is it active?
	// Make sure the hostname member exists
	member := m.GetMemberByHostname(hostname)
	if member == nil {
		log.Warningf("Unknown hostname %s give in call to promoteMember", hostname)
		return errors.New("the specified host does not exist in the configured cluster")
	}
	// if unavailable check it works or do nothing?
	switch member.getStatus() {
	case p.MemberStatus_UNAVAILABLE:
		//If we are the only node and just configured we will be unavailable
		if gconf.nodeCount() > 1 {
			log.Warningf("Unable to promote member %s because it is unavailable", member.getHostname())
			return errors.New("unable to promote member as it is unavailable")
		}
	case p.MemberStatus_ACTIVE:
		log.Warningf("Unable to promote member %s as it is active", member.getHostname())
		return errors.New("unable to promote member as it is already active")
	}
	// get the current active member
	_, activeMember := m.getActiveMember()
	// handle if we do not have an active member
	if activeMember != nil {
		// Make the current Active appliance passive
		success := activeMember.makePassive()
		if !success {
			log.Warningf("Failed to make %s passive, continuing", activeMember.getHostname())
		}
		// TODO: Note: Do we need this?
		// Update our local value for the active member
		activeMember.setStatus(p.MemberStatus_PASSIVE)
	}
	// make the hostname the new active
	success := member.makeActive()
	// make new node active
	if !success {
		log.Warningf("Failed to promote %s to active. Falling back to %s", member.getHostname(), activeMember.getHostname())
		// Somethings gone wrong.. attempt to make the previous active - active again.
		success := activeMember.makeActive()
		if !success {
			log.Error("Failed to make reinstate the active node. Something is really wrong")
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
func (m *Memberlist) monitorClientConns() bool {
	// make sure we are still the active appliance
	member, err := m.getLocalMember()
	if err != nil {
		log.Debug("Memberlist:monitorClientConns() Client monitoring has stopped as it seems we are no longer in a cluster")
		return true
	}
	if member.getStatus() == p.MemberStatus_PASSIVE {
		log.Debug("Memberlist:monitorClientConns() Client monitoring has stopped as we are no longer active")
		return true
	}
	for _, member := range m.Members {
		if member.getHostname() == gconf.getLocalNode() {
			continue
		}
		member.Connect()
		log.Debug(member.Hostname + " connection status is " + member.Connection.GetState().String())
		switch member.Connection.GetState() {
		case connectivity.Idle:
		case connectivity.Ready:
			member.setStatus(p.MemberStatus_PASSIVE)
		default:
			member.setStatus(p.MemberStatus_UNAVAILABLE)
		}
	}
	return false
}

/**
Send health checks to users who have a healthy connection
*/
func (m *Memberlist) addHealthCheckHandler() bool{
	// make sure we are still the active appliance
	member, err := m.getLocalMember()
	if err != nil {
		log.Debug("Memberlist:addHealthCheckhandler() Health check handler has stopped as it seems we are no longer in a cluster")
		return true
	}
	if member.getStatus() == p.MemberStatus_PASSIVE {
		log.Debug("Memberlist:addHealthCheckHandler() Health check handler has stopped as it seems we are no longer active")
		return true
	}
	for _, member := range m.Members {
		if member.getHostname() == gconf.getLocalNode() {
			continue
		}
		if !member.getHCBusy() && member.getStatus() == p.MemberStatus_PASSIVE {
			memberlist := new(p.PulseHealthCheck)
			for _, member := range m.Members {
				newMember := &p.MemberlistMember{
					Hostname: member.getHostname(),
					Status:   member.getStatus(),
					Latency: member.getLatency(),
					LastReceived: member.getLastHCResponse().Format(time.RFC1123),
				}
				memberlist.Memberlist = append(memberlist.Memberlist, newMember)
			}
			go member.routineHC(memberlist)
		}
	}
	return false
}

/**
Sync local config with each member in the cluster.
*/
func (m *Memberlist) SyncConfig() error {
	log.Debug("Memberlist:SyncConfig Syncing config with peers..")
	// Return with our new updated config
	buf, err := json.Marshal(gconf.GetConfig())
	// Handle failure to marshal config
	if err != nil {
		return errors.New("unable to sync config " + err.Error())
	}
	m.Broadcast(SendConfigSync, &p.PulseConfigSync{
		Replicated: true,
		Config:     buf,
	})
	return nil
}

/**
Update the local memberlist statuses based on the proto memberlist message
*/
func (m *Memberlist) update(memberlist []*p.MemberlistMember) {
	log.Debug("Memberlist:update() Updating memberlist")
	m.Lock()
	defer m.Unlock()
	 //do not update the memberlist if we are active
	for _, member := range memberlist {
		for _, localMember := range m.Members {
			if member.GetHostname() == localMember.getHostname() {
				localMember.setStatus(member.Status)
				localMember.setLatency(member.Latency)
				// our local last received has priority
				if member.GetHostname() != gconf.getLocalNode() {
					tym, _ := time.Parse(time.RFC1123, member.LastReceived)
					localMember.setLastHCResponse(tym)
				}
				break
			}
		}
	}
}

/**
Calculate who's next to become active in the memberlist
*/
func (m *Memberlist) getNextActiveMember() (*Member, error) {
	for hostname, _ := range gconf.Nodes {
		member := m.GetMemberByHostname(hostname)
		if member == nil {
			panic("Memberlist:getNextActiveMember() Cannot get member by hostname " + hostname)
		}
		if member.getStatus() == p.MemberStatus_PASSIVE {
			log.Debug("Memberlist:getNextActiveMember() " + member.getHostname() + " is the new active appliance")
			return member, nil
		}
	}
	return &Member{}, errors.New("Memberlist:getNextActiveMember() No new active member found")
}

/**

*/
func (m *Memberlist) getLocalMember() (*Member, error) {
	for _, member := range m.Members {
		if member.getHostname() == gconf.getLocalNode() {
			return member, nil
		}
	}
	return &Member{}, errors.New("cannot get local member. Perhaps we are no longer in a cluster")
}

/**
Reset the memberlist when we are no longer in a cluster.
 */
func (m *Memberlist) reset() {
	m.Lock()
	defer m.Unlock()
	m.Members = []*Member{}
}