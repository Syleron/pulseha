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
	Name   string
	Client Client
}

/**
 *
 */
func (m *Memberlist) AddMember(hostname string, client Client) {
	newMember := &Member{
		Name: hostname,
		Client:client,
	}

	m.Members = append(m.Members, newMember)
}

/**
 *
 */
func (m *Memberlist) RemoveMemberByName(hostname string) () {
	for i, member := range m.Members {
		if member.Name == hostname {
			m.Members = append(m.Members[:i], m.Members[i+1:]...)
		}
	}
}

/**
 *
 */
func (m *Memberlist) GetMemberByHostname(hostname string) (*Member) {
	for _, member := range m.Members {
		if member.Name == hostname {
			return member
		}
	}
	return nil
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


