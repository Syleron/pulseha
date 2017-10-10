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
	"github.com/Syleron/PulseHA/src/utils"
	"github.com/coreos/go-log/log"
	p "github.com/Syleron/PulseHA/proto"
	"sync"
	"encoding/json"
	"runtime"
	"google.golang.org/grpc/connectivity"
	"time"
)

/**
 * Memberlist struct type
 */
type Memberlist struct {
	Members []*Member
	sync.Mutex
}

func (m *Memberlist) Lock() {
	_, _, no, _ := runtime.Caller(1)
	log.Debugf("Memberlist:Unlock() Lock set line: %d", no)
	m.Mutex.Lock()
}

func (m *Memberlist) Unlock() {
	_, _, no, _ := runtime.Caller(1)
	log.Debugf("Memberlist:Unlock() Unlock set line: %d", no)
	m.Mutex.Unlock()
}

/**
 * Add a member to the client list
 */
func (m *Memberlist) MemberAdd(hostname string, client *Client) {
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
		log.Warning("Memberlist:MemberAdd() Member " + hostname + " already exists. Skipping.")
	}
}

/**
 * Remove a member from the client list by hostname
 */
func (m *Memberlist) MemberRemoveByName(hostname string) () {
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
func (m *Memberlist) GetMemberByHostname(hostname string) (*Member) {
	m.Lock()
	defer m.Unlock()
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
func (m *Memberlist) MemberExists(hostname string) (bool) {
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
		if member.hostname == utils.GetHostname() {
			continue
		}
		log.Debugf("Broadcast: %s to member %s", funcName.String(), member.hostname)
		member.Connect()
		member.Send(funcName, data)
		member.Close()
	}
}

/**
 * check how many are in the cluster
 * if more than one, request who's active.
 * if no one responds assume active.
 * This should probably populate the memberlist
 */
func (m *Memberlist) Setup() {
	// Load members into our memberlist slice
	m.LoadMembers()
	// Check to see if we are in a cluster
	if gconf.ClusterCheck() {
		// Are we the only member in the cluster?
		if gconf.ClusterTotal() == 1 {
			// We are the only member in the cluster so
			// we are assume that we are now the active appliance.
			m.PromoteMember(utils.GetHostname())
		} else {
			// Contact a member in the list to see who is the "active" node.
			// Iterate through the memberlist until a response is receive.
			// If no response has been made then assume we should become the active appliance.
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
		m.MemberAdd(key, newClient)
	}
}

/**

 */
func (m *Memberlist) ReloadMembers() {
	log.Debug("Memberlist:ReloadMembers() Reloading member nodes")
	// Do a config reload
	gconf.Reload()
	// clear local members
	m.LoadMembers()
}

/**
	Attempt to connect to all nodes within the memberlist.
 */
func (m *Memberlist) MembersConnect() {
	m.Lock()
	defer m.Unlock()
	for _, member := range m.Members {
		// Make sure we are not connecting to ourself!
		if member.getHostname() != utils.GetHostname() {
			node, err := NodeGetByName(member.getHostname())
			if err != nil {
				log.Warning(member.getHostname() + " could not be found.")
			}
			err = member.Client.Connect(node.IP, node.Port, member.getHostname())
			if err != nil {
				continue
			}
			member.setStatus(p.MemberStatus_PASSIVE)
		}
	}
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
func (m *Memberlist) getActiveMember() string {
	for _, member := range m.Members {
		if member.getStatus() == p.MemberStatus_ACTIVE {
			return member.getHostname()
		}
	}
	return ""
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
	member := m.GetMemberByHostname(hostname)
	if member == nil {
		log.Errorf("Unknown hostname %s give in call to promoteMember", hostname)
		return errors.New("unknown hostname")
	}
	// if unavailable check it works or do nothing?
	switch member.getStatus() {
	case p.MemberStatus_UNAVAILABLE:
		//If we are the only node and just configured we will be unavailable
		if gconf.nodeCount() > 1 {
			log.Errorf("Unable to promote member %s because it is unavailable", member.getHostname())
			return errors.New("unable to promote member because it is unavailable")
		}
	case p.MemberStatus_ACTIVE:
		log.Errorf("Unable to promote member %s because it is active", member.getHostname())
		return nil
	}

	// make current active node passive
	activeMember := m.GetMemberByHostname(m.getActiveMember())
	if activeMember != nil {
		if !activeMember.makePassive() {
			log.Errorf("Failed to make %s passive, continuing", activeMember.getHostname())
		}
		activeMember.status = p.MemberStatus_PASSIVE
	}

	// make new node active
	if !member.makeActive() {
		log.Errorf("Failed to promote %s to active. Falling back to %s", member.getHostname(), activeMember.getHostname())

		if !activeMember.makeActive() {
			log.Error("Failed to make reinstate the active node. Something is really wrong")
		}
	} else {
		// Start performing health checks
		//log.Debug("Memberlist:PromoteMember() Starting RPC health check scheduler..")
		go utils.Scheduler(m.checkConnections, 1 * time.Second)
	}

	//Dont think we need this here
	//m.SyncConfig()
	return nil
}

/**
	Function is only to be run on the active appliance
	Note: THis is not the final function name.. or not sure if this is
          where this logic will stay.. just playing around at this point.
 */
func (m *Memberlist) checkConnections() {
	for _, member := range m.Members {
		go func() {
			if member.hostname == gconf.localNode {
				return
			}
			err := member.Connect()
			if err != nil {
				member.setStatus(p.MemberStatus_UNAVAILABLE)
				return
			} else if member.Connection.GetState() == connectivity.TransientFailure {
				member.setStatus(p.MemberStatus_UNAVAILABLE)
				return
			}
			member.setStatus(p.MemberStatus_PASSIVE)
		}()
	}
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
