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
	"errors"
	"github.com/google/uuid"
	"github.com/labstack/gommon/log"
	"github.com/syleron/pulseha/packages/config"
	"github.com/syleron/pulseha/packages/language"
	"github.com/syleron/pulseha/packages/network"
	"github.com/syleron/pulseha/packages/utils"
)

// nodeCreateLocal Create a new node definition in the config.
func nodeCreateLocal(ip string, port string, assignGroups bool) (string, *config.Node, error) {
	log.Debug("create localhost node config definition")
	hostname, err := utils.GetHostname()
	if err != nil {
		return "", &config.Node{}, errors.New(language.CLUSTER_CREATE_FAIL_HOSTNAME)
	}
	// Define new node object
	newNode := &config.Node{
		IP:       ip,
		Port:     port,
		IPGroups: make(map[string][]string, 0),
		Hostname: hostname,
	}
	// Add the new node
	uid := uuid.New()
	if err := nodeAdd(uid.String(), newNode); err != nil {
		return "", &config.Node{}, errors.New(language.CLUSTER_NODE_ADD_FAIL)
	}
	// Set our local node UUID
	if err := DB.Config.UpdateValue("local_node", uid.String()); err != nil {
		return "", &config.Node{}, err
	}
	// Save our config as we have added our local node
	if err := DB.Config.Save(); err != nil {
		return "", &config.Node{}, err
	}
	// Create interface definitions each with their own group
	for _, ifaceName := range network.GetInterfaceNames() {
		if ifaceName != "lo" {
			newNode.IPGroups[ifaceName] = make([]string, 0)
			if assignGroups {
				groupName := genGroupName()
				DB.Config.Groups[groupName] = []string{}
				if err := groupAssign(groupName, uid.String(), ifaceName); err != nil {
					log.Warnf("Unable to assign group to interface: %s", err.Error())
				}
			}
		}
	}
	// return our results
	return uid.String(), newNode, nil
}

// nodeUpdateLocalInterfaces rewrites the available network interfaces to the config
// Note: This is used when additional interfaces have been added and need to be accounted
//        for.
func nodeUpdateLocalInterfaces() error {
	localHostname, err := utils.GetHostname()
	if err != nil {
		return err
	}
	uid, localNode, err := nodeGetByHostname(localHostname)
	if err != nil {
		return err
	}
	// Get our local interfaces
	localifaces := DB.Config.Nodes[uid].IPGroups // TODO: Should be a config getter
	// Get our current interfaces
	ifaces := network.GetInterfaceNames()
	// Add missing interfaces
	for _, b := range ifaces {
		exist := false
		for n := range localifaces {
			if n == b {
				exist = true
				break
			}
		}
		if !exist && b != "lo" {
			localNode.IPGroups[b] = make([]string, 0)
			groupName := genGroupName()
			DB.Config.Groups[groupName] = []string{}
			if err := groupAssign(groupName, uid, b); err != nil {
				log.Warnf("Unable to assign group to interface: %s", err.Error())
			}
		}
	}
	// Delete missing interfaces
	for b := range localifaces {
		exist := false
		for _, n := range ifaces {
			if n == b {
				exist = true
				break
			}
		}
		if !exist {
			delete(DB.Config.Nodes[uid].IPGroups, b)
		}
	}
	// Save to our config
	if err := DB.Config.Save(); err != nil {
		return errors.New(language.CLUSTER_CONFIG_FAIL)
	}
	return nil
}

// nodeAdd Add a new node to our config.
func nodeAdd(uid string, node *config.Node) error {
	DB.Logging.Debug(uid + " added to local cluster config")
	if !nodeExistsByUUID(uid) {
		DB.Config.Lock()
		DB.Config.Nodes[uid] = node
		DB.Config.Unlock()
		return nil
	}
	return errors.New(language.CLUSTER_NODE_EXISTS)
}

// nodeDelete Remove a node from our config by hostname.
func nodeDelete(uid string) error {
	DB.Logging.Debug("Nodes:nodeDelete()" + uid + " node removed.")
	if nodeExistsByUUID(uid) {
		DB.Config.Lock()
		delete(DB.Config.Nodes, uid)
		DB.Config.Unlock()
		return nil
	}
	return errors.New(language.CLUSTER_NODE_NOTFOUND)
}

// nodesClearLocal Clear out local nodes section from the config.
func nodesClearLocal() {
	DB.Logging.Debug("All nodes cleared from local config")
	DB.Config.Lock()
	DB.Config.Nodes = map[string]*config.Node{}
	DB.Config.Unlock()
}

// nodeExistsByHostname Determines whether a node already exists in our config by hostname.
func nodeExistsByHostname(hostname string) bool {
	for _, node := range DB.Config.Nodes {
		if node.Hostname == hostname {
			return true
		}
	}
	return false
}

/**
* Determines whether a Node already exists in a config based
  off the nodes UUID.
*/

// nodeExistsByUUID
func nodeExistsByUUID(uid string) bool {
	for key := range DB.Config.Nodes {
		if key == uid {
			return true
		}
	}
	return false
}

/**
Get node by its hostname
*/

// nodeGetbyUUID
func nodeGetByUUID(uid string) (config.Node, error) {
	for key, node := range DB.Config.Nodes {
		if key == uid {
			return *node, nil
		}
	}
	return config.Node{}, errors.New(language.CLUSTER_NODE_NOTFOUND)
}

// nodeGetHostname Get node object by hostnam
func nodeGetByHostname(hostname string) (string, config.Node, error) {
	for uid, node := range DB.Config.Nodes {
		if node.Hostname == hostname {
			return uid, *node, nil
		}
	}
	return "", config.Node{}, errors.New(language.CLUSTER_NODE_NOTFOUND)
}

// nodeAssignedToInterface Checks to see if a node has any interface assignments.
func nodeAssignedToInterface(group string) bool {
	for _, node := range DB.Config.Nodes { // :-|
		for _, groups := range node.IPGroups { // :-s
			for _, ifaceGroup := range groups { // :-(
				if ifaceGroup == group {
					return true
				}
			}
		}
	}
	return false
}

// nodeInterfaceGroupExists Checks to see if a floating IP group has already been assigned
//                          to a node's interface.
func nodeInterfaceGroupExists(uid, iface, group string) (bool, int) {
	for index, existingGroup := range DB.Config.Nodes[uid].IPGroups[iface] {
		if existingGroup == group {
			return true, index
		}
	}
	return false, -1
}

// nodeUpdateLocalHostname update our node hostname in our config
// Note: This will resync the config.
func nodeUpdateLocalHostname(hostname string) error {
	// Set our local value
	DB.Config.UpdateHostname(hostname)
	// Save our changes
	if err := DB.Config.Save(); err != nil {
		log.Error("Unable to save local config. This likely means the local config is now out of date.")
	}
	// Resync the config
	if err := DB.MemberList.SyncConfig(); err != nil {
		return err
	}
	return nil
}
