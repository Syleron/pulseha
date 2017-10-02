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
	"github.com/Syleron/PulseHA/proto"
	"sync"
)

/**
 * Memberlist struct type
 */
type Memberlist struct {
	Members []*Member
	sync.Mutex
}

/**
 * Member struct type
 */
type Member struct {
	hostname   string
	status proto.MemberStatus_Status
	Client
	sync.Mutex
}

/*
 Getters and setters for Member which allow us to make them go routine safe
 */

func (m *Member) getHostname()string {
	m.Lock()
	defer m.Unlock()
	return m.hostname
}
func (m *Member) setHostname(hostname string){
	m.Lock()
	defer m.Unlock()
	m.hostname = hostname
}

func (m *Member) getStatus()proto.MemberStatus_Status {
	m.Lock()
	defer m.Unlock()
	return m.status
}

func (m *Member) setStatus(status proto.MemberStatus_Status) {
	m.Lock()
	defer m.Unlock()
	m.status = status
}
func (m *Member) setClient(client Client) {
	m.Client = client
}
/**
 * Add a member to the client list
 */
func (m *Memberlist) MemberAdd(hostname string, client *Client) {
	m.Lock()
	defer m.Unlock()
	newMember := &Member{}

	newMember.setHostname(hostname)
	newMember.setStatus(proto.MemberStatus_UNAVAILABLE)
	newMember.setClient(*client)
	
	m.Members = append(m.Members, newMember)
}

/**
 * Remove a member from the client list by hostname
 */
func (m *Memberlist) MemberRemoveByName(hostname string) () {
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
func (m *Memberlist) Broadcast(funcName string, params ... interface{}) (interface{}, error) {
	m.Lock()
	defer m.Unlock()
	for _, member := range m.Members {
		funcList := member.GetFuncBroadcastList()
		f := reflect.ValueOf(funcList[funcName])
		if len(params) != f.Type().NumIn() {
			return nil, errors.New("the number of passed parameters do not match the function")
		}
		vals := make([]reflect.Value, len(params))
		for k, param := range params {
			vals[k] = reflect.ValueOf(param)
		}
		f.Call(vals)
	}
	return nil, nil
}

/**
 * check how many are in the cluster
 * if more than one, request who's active.
 * if no one responds assume active.
 * This should probably populate the memberlist
 */
func (m *Memberlist) Setup() {
	// todo work out hwo to handle mutex in here
	// Load members into our memberlist slice
	m.LoadMembers()
	// Check to see if we are in a cluster
	if clusterCheck() {
		// Are we the only member in the cluster?
		if clusterTotal() == 1 {
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
		log.Debug("Memberlist:LoadMembers() " + key + " added to memberlist")
		newClient := &Client{}
		m.MemberAdd(key, newClient)
	}
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
			member.setStatus(proto.MemberStatus_PASSIVE)
		}
	}
}

/**
	Get status of a specific member by hostname
 */
func (m *Memberlist) MemberGetStatus(hostname string) (proto.MemberStatus_Status, error) {
	m.Lock()
	defer m.Unlock()
	for _, member := range m.Members {
		if member.getHostname() == hostname {
			return member.getStatus(), nil
		}
	}
	return proto.MemberStatus_UNAVAILABLE, errors.New("unable to find member with hostname " + hostname)
}

/**
	Promote a member within the memberlist to become the active
	node
 */
func (m *Memberlist) PromoteMember(hostname string)error {
	m.Lock()
	defer m.Unlock()
	// Inform everyone in the cluster that a specific node is now the new active
	// Demote if old active is no longer active. promote if the passive is the new active.

	// get host is it active?
	node := m.GetMemberByHostname(hostname)
	if node == nil {
		log.Errorf("Unknown hostname % give in call to promoteMember",hostname)
		return errors.New("Unknown hostname")
	}
	// if unavailable check it works or do nothing?


	// make current active node passive
	// make new node active
	// update all members

	return nil
}

/**
	Sync local config with each member in the cluster.
 */
func (m *Memberlist) SyncConfig() {
	//config := gconf.GetConfig()
}


