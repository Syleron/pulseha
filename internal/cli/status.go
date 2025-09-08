package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/spf13/cobra"
	"github.com/syleron/pulseha/internal/client"
	rpc "github.com/syleron/pulseha/rpc"
)

func NewStatusCmd() *cobra.Command {
	var jsonOutput bool

	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show cluster status",
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := client.New()
			if err != nil {
				return err
			}

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			resp, err := client.CLI().Status(ctx, &rpc.StatusRequest{})
			if err != nil {
				return err
			}

			// Translate RPC to client.ClusterStatus
			status, convErr := translateStatusResponse(resp)
			if convErr != nil {
				return convErr
			}

			if jsonOutput {
				output, err := json.MarshalIndent(status, "", "  ")
				if err != nil {
					return err
				}
				fmt.Println(string(output))
				return nil
			}

			// Pretty print status
			return printClusterStatus(status)
		},
	}

	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Output in JSON format")

	return cmd
}

func translateStatusResponse(resp *rpc.StatusResponse) (*client.ClusterStatus, error) {
	c, err := client.New()
	if err != nil {
		return nil, err
	}
	cfg := c.GetConfig()
	status := &client.ClusterStatus{
		Members: make([]client.Member, len(resp.Members)),
		Groups:  make([]client.GroupInfo, 0),
		Mode:    cfg.Pulse.Mode,
	}
	for i, m := range resp.Members {
		s := "Unknown"
		switch m.Status {
		case rpc.MemberStatusEnum_MEMBER_STATUS_ACTIVE:
			s = "Active"
		case rpc.MemberStatusEnum_MEMBER_STATUS_PASSIVE:
			s = "Passive"
		case rpc.MemberStatusEnum_MEMBER_STATUS_PARTIAL_ACTIVE:
			s = "PartialActive"
		}
		status.Members[i] = client.Member{
			Hostname:      m.Hostname,
			IP:            m.Ip,
			Port:          m.Port,
			Status:        s,
			IPs:           m.ActiveIps,
			ActiveIPs:     m.ActiveIps,
			LastResponse:  m.LastResponse,
			Latency:       m.Latency,
			PartialActive: m.PartialActive,
		}
	}
	for groupName, ips := range cfg.Groups {
		gi := client.GroupInfo{Name: groupName, IPs: ips}
		for id, node := range cfg.Nodes {
			for iface, groups := range node.IPGroups {
				for _, g := range groups {
					if g == groupName {
						gi.Assignments = append(gi.Assignments, client.GroupAssignment{NodeID: id, Interface: iface})
					}
				}
			}
		}
		status.Groups = append(status.Groups, gi)
	}
	return status, nil
}

func printClusterStatus(status *client.ClusterStatus) error {
	fmt.Printf("\nCluster Status:\n")
	fmt.Printf("Mode: %s\n", status.Mode)
	fmt.Printf("==============\n")

	// Print live member information
	fmt.Printf("\nLive Members:\n")
	fmt.Printf("-------------\n")
	for _, member := range status.Members {
		fmt.Printf("\nNode: %s\n", member.Hostname)
		fmt.Printf("Address: %s:%s\n", member.IP, member.Port)
		fmt.Printf("Status: %s\n", member.Status)
		if len(member.ActiveIPs) > 0 {
			fmt.Printf("Active IPs: %v\n", member.ActiveIPs)
		}
		if member.PartialActive {
			fmt.Printf("Partially Active: Yes\n")
		}
		if member.LastResponse != "" {
			fmt.Printf("Last Response: %s\n", member.LastResponse)
		}
		if member.Latency != "" {
			fmt.Printf("Latency: %s\n", member.Latency)
		}
	}

	// Derive configured-but-not-live nodes
	liveHosts := make(map[string]struct{})
	liveAddr := make(map[string]struct{})
	for _, m := range status.Members {
		if m.Hostname != "" {
			liveHosts[m.Hostname] = struct{}{}
		}
		if m.IP != "" && m.Port != "" {
			liveAddr[m.IP+":"+m.Port] = struct{}{}
		}
	}

	// Fetch configured nodes from current config
	c, err := client.New()
	if err == nil {
		cfg := c.GetConfig()
		var printedHeader bool
		for id, node := range cfg.Nodes {
			_, hostLive := liveHosts[node.Hostname]
			_, addrLive := liveAddr[node.IP+":"+node.Port]
			if hostLive || addrLive {
				continue
			}
			if !printedHeader {
				fmt.Printf("\nConfigured Nodes (not live):\n")
				fmt.Printf("---------------------------\n")
				printedHeader = true
			}
			fmt.Printf("\nNode ID: %s\n", id)
			fmt.Printf("Hostname: %s\n", node.Hostname)
			fmt.Printf("Configured Address: %s:%s\n", node.IP, node.Port)
			// Hint if address conflicts with a live member
			if _, conflict := liveAddr[node.IP+":"+node.Port]; conflict {
				fmt.Printf("Hint: Address conflicts with a live member at %s:%s\n", node.IP, node.Port)
			}
		}
	}

	// Print group information
	if len(status.Groups) > 0 {
		fmt.Printf("\nFloating IP Groups:\n")
		fmt.Printf("------------------\n")

		for _, group := range status.Groups {
			fmt.Printf("\nGroup: %s\n", group.Name)

			// Print IPs in the group
			if len(group.IPs) > 0 {
				fmt.Printf("  IPs:\n")
				for _, ip := range group.IPs {
					fmt.Printf("    - %s\n", ip)
				}
			} else {
				fmt.Printf("  IPs: None\n")
			}

			// Print assignments
			if len(group.Assignments) > 0 {
				fmt.Printf("  Assigned to:\n")
				for _, assignment := range group.Assignments {
					fmt.Printf("    - Node: %s, Interface: %s\n", assignment.NodeID, assignment.Interface)
				}
			} else {
				fmt.Printf("  Assigned to: None\n")
			}
		}
	}

	return nil
}
