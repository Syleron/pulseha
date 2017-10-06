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

import  (
	"sync"
	"github.com/coreos/go-log/log"
	"github.com/Syleron/PulseHA/proto"
)
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

/*
	Make the node active (bring up its groups)
 */
func (m *Member) makeActive()bool{
	log.Debugf("Making active %s", m.getHostname())
	//get all groups send
	//r, err := m.SendMakeActive()
	log.Debug("member make active" + m.hostname + gconf.getLocalNode())


	if m.hostname == gconf.getLocalNode() {
		log.Debug("member make active" + m.hostname + gconf.getLocalNode())
		makeMemberActive()
	} else {
		err := m.SendMakeActive(&proto.PulsePromote{false,"", m.getHostname()})
		if err != nil {
			log.Error(err)
			log.Errorf("Error making %s active. Error: %s", m.getHostname(), err.Error())
			return false
		}
	}
	return true
}

/**
	Make the node passive (take down its groups)
 */
func (m *Member) makePassive()bool {
	log.Debugf("Making passive %s", m.getHostname())
	return true
}