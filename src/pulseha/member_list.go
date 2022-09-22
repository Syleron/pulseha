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

package pulseha

import (
	"encoding/json"
	"errors"
	log "github.com/sirupsen/logrus"
	"github.com/syleron/pulseha/packages/client"
	"github.com/syleron/pulseha/packages/utils"
	"github.com/syleron/pulseha/rpc"
	"google.golang.org/grpc/connectivity"
	"sync"
	"time"
)

// MemberList defines our member list object
type MemberList struct {
	// Our array of members
	Members []*Member

	// A channel to stop our routines
	StopChan chan bool

	// Our mutex to lock/unlock our object
	sync.Mutex
}

// Lock used to lock our member list object
func (m *MemberList) Lock() {
	m.Mutex.Lock()
}

// Unlock used to unlock our member list object
func (m *MemberList) Unlock() {
	m.Mutex.Unlock()
}

// AddMember adds a member to our member list by hostname
func (m *MemberList) AddMember(hostname string, client *client.Client) {
	if !m.MemberExists(hostname) {
		DB.Logging.Debug("MemberList:MemberAdd() " + hostname + " added to memberlist")
		m.Lock()
		newMember := &Member{}
		newMember.SetHostname(hostname)
		newMember.SetStatus(rpc.MemberStatus_UNAVAILABLE)
		newMember.SetClient(client)
		newMember.SetScore(0)
		m.Members = append(m.Members, newMember)
		m.Unlock()
	} else {
		DB.Logging.Debug("MemberList:MemberAdd() Member " + hostname + " already exists. Skipping.")
	}
}

// MemberRemoveByHostname removes a member by hostname from our member list
func (m *MemberList) MemberRemoveByHostname(hostname string) {
	DB.Logging.Debug("MemberList:MemberRemoveByName() " + hostname + " removed from the memberlist")
	m.Lock()
	defer m.Unlock()
	for i, member := range m.Members {
		if member.GetHostname() == hostname {
			m.Members = append(m.Members[:i], m.Members[i+1:]...)
		}
	}
}

// GetMemberByHostname returns a member object by hostname
func (m *MemberList) GetMemberByHostname(hostname string) *Member {
	m.Lock()
	defer m.Unlock()
	if hostname == "" {
		DB.Logging.Warn("MemberList:GetMemberByHostname() Unable to get member by hostname as hostname is empty!")
	}
	for _, member := range m.Members {
		if member.GetHostname() == hostname {
			return member
		}
	}
	return nil
}

// MemberExists check by hostname if member exists in our member list.
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

// Broadcast sends a RPC message to all members in our member list
func (m *MemberList) Broadcast(funcName client.ProtoFunction, data interface{}) []error {
	DB.Logging.Debug("MemberList:Broadcast() Broadcasting " + funcName.String())
	m.Lock()
	defer m.Unlock()
	var errs []error
	for _, member := range m.Members {
		// We don't want to broadcast to our self!
		hostname, err := utils.GetHostname()
		if err != nil {
			log.Error("cannot broadcast as unable to get local hostname")
			errs = append(errs, errors.New("unable to broadcast as unable to get local hostname"))
			continue
		}
		if member.GetHostname() == hostname {
			continue
		}
		DB.Logging.Debug("Broadcast: " + funcName.String() + " to member " + member.GetHostname())
		if err := member.Connect(); err != nil {
			log.Error("Broadcast() ", err)
			errs = append(errs, err)
			continue
		}
		if _, err := member.Send(funcName, data); err != nil {
			log.Error("Broadcast()", err)
			errs = append(errs, err)
			continue
		}
	}
	return errs
}

// Setup bootstraps our member list.
// 1) Loads our member list from memory.
// 2) Checks whether we are in a cluster or not.
// 3) Determines if we are in a cluster of 1 and whether we should go active.
func (m *MemberList) Setup() {
	// Load members into our member list slice
	m.LoadMembers()
	// Check to see if we are in a cluster
	localNode, err := DB.Config.GetLocalNode()
	// We cannot setup as we don't have a local node.
	if err != nil {
		log.Fatal(err)
	}
	if DB.Config.ClusterCheck() {
		// Start out health check scheduler
		hcs := HealthChecks{}
		// TODO: Better way of doing this
		hcs.Plugins = DB.Plugins.GetHealthCheckPlugins()
		go utils.Scheduler(
			hcs.ProcessHCs,
			time.Duration(5)*time.Second,
		)
		//fmt.Println(">>>>> ", <-hcs.ScoreChan)
		// Are we the only member in the cluster?
		if DB.Config.NodeCount() == 1 {
			// Disable start up delay
			DB.StartDelay = false
			// We are the only member in the cluster so
			// we are assume that we are now the active appliance.
			m.PromoteMember(localNode.Hostname)
		} else {
			// come up passive and monitoring health checks
			localMember := m.GetMemberByHostname(localNode.Hostname)
			localMember.SetLastHCResponse(time.Now())
			localMember.SetStatus(rpc.MemberStatus_PASSIVE)
			DB.Logging.Debug("MemberList:Setup() starting the monitor received health checks scheduler")
			go utils.Scheduler(
				localMember.MonitorReceivedHCs,
				time.Duration(DB.Config.Pulse.FailOverInterval)*time.Millisecond,
			)
		}
	}
}

// LoadMembers loads our config members from memory into our member list
func (m *MemberList) LoadMembers() {
	for _, node := range DB.Config.Nodes {
		newClient := &client.Client{}
		m.AddMember(node.Hostname, newClient)
	}
}

// Reload resets our member list based on what's in our written config
// Note: This will reload our config from file.
func (m *MemberList) Reload() {
	DB.Logging.Debug("MemberList:ReloadMembers() Reloading member nodes")
	// Reload our config
	DB.Config.Reload()
	// clear local members
	m.LoadMembers()
}

// MemberGetStatus returns the status for a member by hostname.
func (m *MemberList) MemberGetStatus(hostname string) (rpc.MemberStatus_Status, error) {
	m.Lock()
	defer m.Unlock()
	for _, member := range m.Members {
		if member.GetHostname() == hostname {
			return member.GetStatus(), nil
		}
	}
	return rpc.MemberStatus_UNAVAILABLE, errors.New("unable to find member with hostname " + hostname)
}

// GetActiveMember returns the hostname of the active member within our cluster.
// Note: This will return empty if no one is active.
func (m *MemberList) GetActiveMember() (string, *Member) {
	for _, member := range m.Members {
		if member.GetStatus() == rpc.MemberStatus_ACTIVE {
			return member.GetHostname(), member
		}
	}
	return "", nil
}

// PromoteMember promotes a member as active within our member list.
func (m *MemberList) PromoteMember(hostname string) error {
	DB.Logging.Debug("MemberList:PromoteMember() MemberList promoting " + hostname + " as active member..")
	// Inform everyone in the cluster that a specific node is now the new active
	// Demote if old active is no longer active. promote if the passive is the new active.
	// get host is it active?

	// Make sure the hostname member exists
	newActive := m.GetMemberByHostname(hostname)

	// Make sure we have a hostname
	if newActive == nil {
		DB.Logging.Warn("Unknown hostname " + hostname + " give in call to promoteMember")
		return errors.New("the specified host does not exist in the configured cluster")
	}

	// if unavailable check it works or do nothing?
	switch newActive.GetStatus() {
	case rpc.MemberStatus_UNAVAILABLE:
		// When we are attempting to promote a node who is unavailable
		// If we are the only node and just configured we will be unavailable
		if DB.Config.NodeCount() > 1 {
			DB.Logging.Warn("Unable to promote member " + newActive.GetHostname() + " because it is unavailable")
			return errors.New("unable to promote member as it is unavailable")
		}
	case rpc.MemberStatus_ACTIVE:
		// When we are attempting to promote the active appliance
		DB.Logging.Warn("Unable to promote member " + newActive.GetHostname() + " as it is active")
		return errors.New("unable to promote member as it is already active")
	}

	// get the current active member
	_, activeMember := m.GetActiveMember()

	// If we do have an active member, make it passive
	if activeMember != nil {
		// Make the current Active appliance passive
		if err := activeMember.MakePassive(); err != nil {
			DB.Logging.Warn("Failed to make " + activeMember.GetHostname() + " passive, continuing")
		}
		// TODO: Note: Do we need this?
		// Update our local value for the active member
		activeMember.SetStatus(rpc.MemberStatus_PASSIVE)
	}

	// make the the new node active
	if err := newActive.MakeActive(); err != nil {
		DB.Logging.Warn("Failed to promote " + newActive.GetHostname() + " to active. Falling back to " + activeMember.GetHostname())
		// Somethings gone wrong.. attempt to make the previous active - active again.
		if err := activeMember.MakeActive(); err != nil {
			DB.Logging.Error("Failed to make reinstate the active node. Something is really wrong")
		}
		// Note: we don't need to update the active status as we should receive an updated memberlist from the active
	}
	return nil
}

// MonitorClientConns ensures the state for each node is correct based on health check responses.
// Type: Active node scheduled function
func (m *MemberList) MonitorClientConns() bool {
	// Clear routine
	if !DB.Config.ClusterCheck() {
		log.Debug("MonitorClientConns() routine cleared")
		return true
	}
	// make sure we are still the active appliance
	localMember, err := m.GetLocalMember()
	if err != nil {
		DB.Logging.Debug("MemberList:monitorClientConns() Client monitoring has stopped as it seems we are no longer in a cluster")
		return true
	}
	if localMember.GetStatus() == rpc.MemberStatus_PASSIVE {
		DB.Logging.Debug("MemberList:monitorClientConns() Client monitoring has stopped as we are no longer active")
		return true
	}
	for _, member := range m.Members {
		if member.GetHostname() == localMember.Hostname {
			continue
		}
		if err := member.Connect(); err != nil {
			DB.Logging.Error(err.Error())
			return false
		}
		DB.Logging.Debug("MemberList:MonitorClientConns() " + member.Hostname + " connection status is " + member.Connection.GetState().String())
		switch member.Connection.GetState() {
		case connectivity.Idle:
		case connectivity.Ready:
			member.SetStatus(rpc.MemberStatus_PASSIVE)
		default:
			member.SetStatus(rpc.MemberStatus_UNAVAILABLE)
		}
	}
	return false
}

// AddHealthCheckHandler sends GRPC health check messages to our member cluster.
// Type: Active node scheduled function
func (m *MemberList) AddHealthCheckHandler() bool {
	// Clear routine
	if !DB.Config.ClusterCheck() {
		log.Debug("AddHealthCheckHandler() routine cleared")
		return true
	}
	// make sure we are still the active appliance
	localMember, err := m.GetLocalMember()
	if err != nil {
		DB.Logging.Debug("MemberList:addHealthCheckhandler() Health check handler has stopped as it seems we are no longer in a cluster")
		return true
	}
	if localMember.GetStatus() == rpc.MemberStatus_PASSIVE {
		DB.Logging.Debug("MemberList:addHealthCheckHandler() Health check handler has stopped as it seems we are no longer active")
		return true
	}
	//make sure we are still the highest scoring member
	highScorer, err := m.GetHighestScoreMember()

	if err != nil {
		// TODO: better handling
		panic(err)
	}

	if highScorer != localMember {
		log.Debug("A different node has a higher score. Promoting...")
		if err := m.PromoteMember(highScorer.Hostname); err != nil {
			log.Debug(err)
		}
		return true
	}
	for _, member := range m.Members {
		if member.GetHostname() == localMember.Hostname {
			continue
		}
		if !member.GetHCBusy() && member.GetStatus() == rpc.MemberStatus_PASSIVE {
			memberlist := new(rpc.HealthCheckRequest)
			for _, member := range m.Members {
				newMember := &rpc.MemberlistMember{
					Hostname:     member.GetHostname(),
					Status:       member.GetStatus(),
					Latency:      member.GetLatency(),
					LastReceived: member.GetLastHCResponse().Format(time.RFC1123),
					Score:        int32(member.Score),
				}
				memberlist.Memberlist = append(memberlist.Memberlist, newMember)
			}
			go member.RoutineHC(memberlist)
		}
	}
	return false
}

// SyncConfig syncs our local config with each member in the cluster.
func (m *MemberList) SyncConfig() error {
	DB.Logging.Debug("MemberList:SyncConfig() Syncing config with peers..")
	// Return with our new updated config
	buf, err := json.Marshal(DB.Config.GetConfig())
	// Handle failure to marshal config
	if err != nil {
		return errors.New("unable to sync config " + err.Error())
	}
	m.Broadcast(client.SendConfigSync, &rpc.ConfigSyncRequest{
		Replicated: true,
		Config:     buf,
	})
	return nil
}

// Update updates the local member list.
// E.g. updates the member list based on data from our proto member message.
func (m *MemberList) Update(memberlist []*rpc.MemberlistMember) {
	DB.Logging.Debug("MemberList:update() Updating memberlist")
	m.Lock()
	defer m.Unlock()
	localNode, err := DB.Config.GetLocalNode()
	if err != nil {
		DB.Logging.Error("MemberList:update() Failed to find local node. Update failed.")
		return
	}
	//do not update the memberlist if we are active
	for _, member := range memberlist {
		for _, localMember := range m.Members {
			if member.GetHostname() == localMember.GetHostname() {
				localMember.SetStatus(member.Status)
				localMember.SetLatency(member.Latency)
				// Don't replace our local score through the hc updates
				if localMember.GetHostname() != localNode.Hostname {
					localMember.SetScore(int(member.Score))
				}
				// our local last received has priority
				if member.GetHostname() != localNode.Hostname {
					tym, _ := time.Parse(time.RFC1123, member.LastReceived)
					localMember.SetLastHCResponse(tym)
				}
				break
			}
		}
	}
}

// GetNextActiveMember calculates who's next to become active in our member list
// TODO: This needs improvements including the use of plugins. Also failback??
func (m *MemberList) GetNextActiveMember() (*Member, error) {
	for _, node := range DB.Config.Nodes {
		member := m.GetMemberByHostname(node.Hostname)
		if member == nil {
			panic("MemberList:getNextActiveMember() Cannot get member by hostname " + node.Hostname)
		}
		if member.GetStatus() == rpc.MemberStatus_PASSIVE {
			log.Debug("MemberList:getNextActiveMember() " + member.GetHostname() + " is the new active appliance")
			return member, nil
		}
	}
	return &Member{}, errors.New("MemberList:getNextActiveMember() No new active member found")
}

// TODO: Check if everyone is the same
func (m *MemberList) GetHighestScoreMember() (*Member, error) {
	var score int = -1
	var winningMember *Member

	// First check to see if we all have the same score
	for _, node := range DB.Config.Nodes {
		member := m.GetMemberByHostname(node.Hostname)

		// Are we the only member?
		if len(DB.Config.Nodes) == 1 {
			return member, nil
		}

		// If this is the first round, set our score and move onto the next
		if score < 0 {
			score = member.Score
			continue
		}

		// Check if our score is different
		if score != member.Score {
			// We are different, reset our score and break from the loop
			score = -1
			break
		}

		// otherwise we are the same! return our current active node.
		_, activeMember := m.GetActiveMember()
		return activeMember, nil
	}

	// otherwise calculate who has the winning score
	for _, node := range DB.Config.Nodes {
		// Get our member by hostname
		member := m.GetMemberByHostname(node.Hostname)

		// Make sure our member is not nil
		if member == nil {
			panic("MemberList:getNextActiveMember() Cannot get member by hostname " + node.Hostname)
		}

		if member.Score > score {
			score = member.Score
			winningMember = member
		}
	}

	// Handle encase we are unable to get a new member for whatever logical reason
	if winningMember == nil {
		return nil, errors.New("failed to find winning member")
	}

	log.Debug("MemberList:GetHighestScoreMember() Winning member: ", winningMember)
	return winningMember, nil
}

// GetLocalMember returns the local member object.
func (m *MemberList) GetLocalMember() (*Member, error) {
	m.Lock()
	defer m.Unlock()
	localNode, err := DB.Config.GetLocalNode()
	if err != nil {
		return &Member{}, err
	}
	for _, member := range m.Members {
		if member.GetHostname() == localNode.Hostname {
			return member, nil
		}
	}
	return &Member{}, errors.New("cannot get local member. Perhaps we are no longer in a cluster")
}

// Reset clears the member list.
func (m *MemberList) Reset() {
	m.Lock()
	defer m.Unlock()
	m.Members = []*Member{}
}
