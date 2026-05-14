package ipam

import (
	"fmt"
	"sync"

	"github.com/syleron/pulseha/internal/membership"
	"github.com/syleron/pulseha/packages/config"
)

// Distributor handles IP distribution across cluster nodes
type Distributor struct {
	sync.RWMutex
	balancer *Balancer
	members  *membership.MemberList
	config   *config.Config
}

// NewDistributor creates a new IP distributor
func NewDistributor(members *membership.MemberList, cfg *config.Config, strategy IPDistributionStrategy) *Distributor {
	return &Distributor{
		balancer: NewBalancer(strategy),
		members:  members,
		config:   cfg,
	}
}

// DistributeIPs handles IP distribution across available nodes
func (d *Distributor) DistributeIPs(group string) error {
	d.Lock()
	defer d.Unlock()

	// Get IPs for this group
	ips := d.config.Groups[group]
	if len(ips) == 0 {
		return nil
	}

	// Get available nodes
	availableNodes := d.getAvailableNodes()
	if len(availableNodes) == 0 {
		return fmt.Errorf("no available nodes for IP distribution")
	}

	// Get assignments from balancer
	assignments := d.balancer.Rebalance(availableNodes, ips)

	// Apply assignments to nodes
	for nodeID, nodeIPs := range assignments {
		member := d.members.GetMemberByHostname(nodeID)
		if member == nil {
			continue
		}

		if err := member.MakeActive(nodeIPs); err != nil {
			return fmt.Errorf("failed to assign IPs to node %s: %v", nodeID, err)
		}
	}

	return nil
}

// getAvailableNodes returns a list of nodes that can accept IP assignments
func (d *Distributor) getAvailableNodes() []string {
	var nodes []string

	membersSnapshot := d.members.MembersSnapshot()
	for _, member := range membersSnapshot {
		// Skip unavailable or maintenance nodes
		if member.Status == membership.StatusUnknown || member.Status == membership.StatusMaintenance {
			continue
		}

		// Passive or Active nodes can accept IP assignments
		if member.Status == membership.StatusPassive || member.Status == membership.StatusActive {
			nodes = append(nodes, member.Hostname)
		}
	}

	return nodes
}

// RebalanceCluster triggers a rebalance of all IP groups
func (d *Distributor) RebalanceCluster() error {
	d.Lock()
	defer d.Unlock()

	for groupName := range d.config.Groups {
		if err := d.DistributeIPs(groupName); err != nil {
			return fmt.Errorf("failed to rebalance group %s: %v", groupName, err)
		}
	}

	return nil
}

// HandleNodeFailure handles redistribution of IPs when a node fails
func (d *Distributor) HandleNodeFailure(failedNode string) error {
	d.Lock()
	defer d.Unlock()

	// Get failed member
	member := d.members.GetMemberByHostname(failedNode)
	if member == nil {
		return fmt.Errorf("failed node %s not found", failedNode)
	}

	// Get IPs that need redistribution
	failedIPs := member.ActiveIPs

	// Get available nodes for redistribution
	availableNodes := d.getAvailableNodes()
	if len(availableNodes) == 0 {
		return fmt.Errorf("no available nodes for IP redistribution")
	}

	// Redistribute IPs
	assignments := d.balancer.Rebalance(availableNodes, failedIPs)

	// Apply new assignments
	for nodeID, ips := range assignments {
		member := d.members.GetMemberByHostname(nodeID)
		if member == nil {
			continue
		}

		if err := member.MakeActive(ips); err != nil {
			return fmt.Errorf("failed to reassign IPs to node %s: %v", nodeID, err)
		}
	}

	return nil
}
