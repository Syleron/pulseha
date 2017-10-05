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
	"reflect"
	"errors"
	"github.com/Syleron/PulseHA/src/utils"
	"github.com/coreos/go-log/log"
	p "github.com/Syleron/PulseHA/proto"
	"sync"
	"encoding/json"
)

/**
 * Memberlist struct type
 */
type Memberlist struct {
	Members []*Member
	sync.Mutex
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
func (m *Memberlist) Broadcast(funcName string, params ... interface{}) (error) {
	log.Debug("Memberlist:Broadcast() Broadcasting " + funcName)
	m.Lock()
	defer m.Unlock()
	for _, member := range m.Members {
		// We don't want to broadcast to our self!
		if member.hostname == utils.GetHostname() {
			continue
		}
		// check to see if we have a connection state
		if member.Connection == nil {
			nodeDetails, _ := NodeGetByName(member.hostname)
			err := member.Connect(nodeDetails.IP, nodeDetails.Port, member.hostname)
			if err != nil {
				log.Warning("Unable to connect to " + member.hostname)
				continue
			}
		}
		funcList := member.GetFuncBroadcastList()
		f := reflect.ValueOf(funcList[funcName])
		if len(params) != f.Type().NumIn() {
			return errors.New("the number of passed parameters do not match the function")
		}
		vals := make([]reflect.Value, len(params))
		for k, param := range params {
			vals[k] = reflect.ValueOf(param)
		}
		value := f.Call(vals)
		// checking the length is probably unnecessary
		if len(value) == 2 {
			err := value[1].Interface().(error)
			if err != nil {
				log.Error(err.Error())
			}
		}
		// TODO: Mark a node dead if it cannot be reached
		if member.Connection == nil {
			member.Close()
		}
	}
	return nil
}

/**
 * check how many are in the cluster
 * if more than one, request who's active.
 * if no one responds assume active.
 * This should probably populate the memberlist
 */
func (m *Memberlist) Setup() {
	// todo work out how to handle mutex in here
	log.Debug("Running member Setup")
	// Load members into our memberlist slice
	m.LoadMembers()
	// Check to see if we are in a cluster
	if gconf.ClusterCheck() {
		// Are we the only member in the cluster?
		if gconf.ClusterTotal() == 1 {
			// We are the only member in the cluster so
			// we are assume that we are now the active appliance.
			//m.PromoteMember(utils.GetHostname())
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
	m.Lock()
	defer m.Unlock()
	// Inform everyone in the cluster that a specific node is now the new active
	// Demote if old active is no longer active. promote if the passive is the new active.

	// get host is it active?
	member := m.GetMemberByHostname(hostname)
	if member == nil {
		log.Errorf("Unknown hostname % give in call to promoteMember", hostname)
		return errors.New("unknown hostname")
	}
	// if unavailable check it works or do nothing?
	switch member.getStatus() {
	case p.MemberStatus_UNAVAILABLE:
		log.Errorf("Unable to promote member %s because it is unavailable", member.getHostname())
		return errors.New("unable to promote member because it is unavailable")
	case p.MemberStatus_ACTIVE:
		log.Errorf("Unable to promote member %s because it is active", member.getHostname())
		return nil
	}

	// make current active node passive
	activeMember := m.GetMemberByHostname(m.getActiveMember())
	if !activeMember.makePassive() {
		log.Errorf("Failed to make % passive, continuing", activeMember.getHostname())
	}
	// make new node active
	if !member.makeActive() {
		log.Errorf("Failed to promote % to active. Falling back to %s", member.getHostname(), activeMember.getHostname())
	}
	// update all members
	m.SyncConfig()

	return nil
}

/**
	Sync local config with each member in the cluster.
 */
func (m *Memberlist) SyncConfig() error {
	// Return with our new updated config
	buf, err := json.Marshal(gconf.GetConfig())
	// Handle failure to marshal config
	if err != nil {
		return errors.New("unable to sync config " + err.Error())
	}
	m.Broadcast("SendConfigSync", &p.PulseConfigSync{
		Replicated: true,
		Config:     buf,
	})
	return nil
}
