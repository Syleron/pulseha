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
)

/**
 * Memberlist struct type
 */
type Memberlist struct {
	Members []*Member
}

/**
 * Member struct type
 */
type Member struct {
	Hostname   string
	Status proto.MemberStatus_Status
	Client Client
}

/**
 * Add a member to the client list
 */
func (m *Memberlist) MemberAdd(hostname string, client *Client) {
	newMember := &Member{
		Hostname: hostname,
		Status: proto.MemberStatus_UNAVAILABLE,
		Client: *client,
	}

	m.Members = append(m.Members, newMember)
}

/**
 * Remove a member from the client list by hostname
 */
func (m *Memberlist) MemberRemoveByName(hostname string) () {
	for i, member := range m.Members {
		if member.Hostname == hostname {
			m.Members = append(m.Members[:i], m.Members[i+1:]...)
		}
	}
}

/**
 * Return Member by hostname
 */
func (m *Memberlist) GetMemberByHostname(hostname string) (*Member) {
	for _, member := range m.Members {
		if member.Hostname == hostname {
			return member
		}
	}
	return nil
}

/**
 * Return true/false whether a member exists or not.
 */
func (m *Memberlist) MemberExists(hostname string) (bool) {
	for _, member := range m.Members {
		if member.Hostname == hostname {
			return true
		}
	}
	return false
}

/**
 * Attempt to broadcast a client function to other nodes (clients) within the memberlist
 */
func (m *Memberlist) Broadcast(funcName string, params ... interface{}) (interface{}, error) {
	for _, member := range m.Members {
		funcList := member.Client.GetFuncBroadcastList()
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
	for key, _ := range config.Nodes {
		log.Debug("Memberlist:LoadMembers() " + key + " added to memberlist")
		newClient := &Client{}
		m.MemberAdd(key, newClient)
	}
}

/**
	Attempt to connect to all nodes within the memberlist.
 */
func (m *Memberlist) MembersConnect() {
	for _, member := range m.Members {
		// Make sure we are not connecting to ourself!
		if member.Hostname != utils.GetHostname() {
			node, err := NodeGetByName(member.Hostname)
			if err != nil {
				log.Warning(member.Hostname + " could not be found.")
			}
			err = member.Client.Connect(node.IP, node.Port, member.Hostname)
			if err != nil {
				continue
			}
			member.Status = proto.MemberStatus_PASSIVE
		}
	}
}

/**
	Get status of a specific member by hostname
 */
func (m *Memberlist) MemberGetStatus(hostname string) (proto.MemberStatus_Status, error) {
	for _, member := range m.Members {
		if member.Hostname == hostname {
			return member.Status, nil
		}
	}
	return proto.MemberStatus_UNAVAILABLE, errors.New("unable to find member with hostname " + hostname)
}

/**
	Promote a member within the memberlist to become the active
	node
 */
func (m *Memberlist) PromoteMember(hostname string) {
	// Inform everyone in the cluster that a specific node is now the new active
	// Demote if old active is no longer active. promote if the passive is the new active.
}

/**
	Sync local config with each member in the cluster.
 */
func (m *Memberlist) SyncConfig() {
	config := gconf.GetConfig()


}


