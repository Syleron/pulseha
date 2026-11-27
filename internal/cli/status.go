package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/spf13/cobra"
	"github.com/syleron/pulseha/internal/client"
	"github.com/syleron/pulseha/packages/utils"
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

			// Calculate cluster health status
			status.ClusterHealth = calculateClusterHealth(status.Members)

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
	mode := resp.Mode
	if mode == "" {
		mode = "active-passive" // backward compat with older daemons
	}
	status := &client.ClusterStatus{
		Members: make([]client.Member, len(resp.Members)),
		Groups:  make([]client.GroupInfo, 0),
		Mode:    mode,
	}
	for i, m := range resp.Members {
		s := "Unknown"
		switch m.Status {
		case rpc.MemberStatusEnum_MEMBER_STATUS_ACTIVE:
			s = "Active"
		case rpc.MemberStatusEnum_MEMBER_STATUS_PASSIVE:
			s = "Passive"
		case rpc.MemberStatusEnum_MEMBER_STATUS_MAINTENANCE:
			s = "Maintenance"
		case rpc.MemberStatusEnum_MEMBER_STATUS_STANDBY:
			s = "Standby"
		case rpc.MemberStatusEnum_MEMBER_STATUS_WITNESS:
			s = "Witness"
		}

		nodeID := m.NodeId

		status.Members[i] = client.Member{
			Hostname:     m.Hostname,
			NodeID:       nodeID,
			IP:           m.Ip,
			Port:         m.Port,
			Status:       s,
			IPs:          m.ActiveIps,
			ActiveIPs:    m.ActiveIps,
			LastResponse: m.LastResponse,
			Latency:      m.Latency,
			Witness:      m.Witness,

			WitnessAddress: m.WitnessAddress,
			WitnessTrusted: m.WitnessTrusted,
		}
	}
	for _, g := range resp.Groups {
		gi := client.GroupInfo{Name: g.Name, IPs: g.Ips}
		for _, a := range g.Assignments {
			gi.Assignments = append(gi.Assignments, client.GroupAssignment{NodeID: a.NodeId, Interface: a.Interface})
		}
		status.Groups = append(status.Groups, gi)
	}
	return status, nil
}

func calculateClusterHealth(members []client.Member) string {
	if len(members) == 0 {
		return "down"
	}

	activeCount := 0
	totalCount := 0

	// Counts reachable nodes, not serving ones. Standby belongs here for the
	// same reason Passive does: the node is up and answering, it just holds no
	// floating IPs. Omitting it would report a fully healthy cluster as
	// "degraded" purely because a node's label changed.
	//
	// A Witness is left out of both counts, whether it is up or down. It is
	// never counted anywhere else, and a cluster whose Witness is down behaves
	// exactly like one that never had a Witness -- which is not degraded.
	for _, member := range members {
		if member.Witness {
			continue
		}
		totalCount++
		switch member.Status {
		case "Active", "Passive", "Standby":
			activeCount++
		}
	}

	if activeCount == 0 {
		return "down"
	} else if activeCount == totalCount {
		return "online"
	} else {
		return "degraded"
	}
}

func printClusterStatus(status *client.ClusterStatus) error {
	fmt.Printf("\nCluster Status: %s\n", status.ClusterHealth)
	fmt.Printf("Mode: %s\n", status.Mode)
	fmt.Printf("==============\n")

	// Sort members by hostname for consistent ordering
	sort.Slice(status.Members, func(i, j int) bool {
		return status.Members[i].Hostname < status.Members[j].Hostname
	})

	// Print node information
	fmt.Printf("\nNodes:\n")
	fmt.Printf("------\n")
	for _, member := range status.Members {
		fmt.Printf("\nNode: %s\n", member.Hostname)
		if member.NodeID != "" {
			fmt.Printf("Node ID: %s\n", member.NodeID)
		}
		fmt.Printf("Address: %s:%s\n", utils.FormatIPv6(member.IP), member.Port)
		fmt.Printf("Status: %s\n", member.Status)
		if line := witnessLine(member, status.Members); line != "" {
			fmt.Println(line)
		}
		if len(member.ActiveIPs) > 0 {
			fmt.Printf("Active IPs: %v\n", member.ActiveIPs)
		}
		if member.Status == "Unknown" || member.LastResponse == "" {
			fmt.Printf("Last Response: N/A\n")
			fmt.Printf("Latency: N/A\n")
		} else {
			if t, err := time.Parse(time.RFC3339, member.LastResponse); err == nil {
				ago := time.Since(t).Round(time.Second)
				if ago < 0 {
					ago = -ago
				}
				fmt.Printf("Last Response: %s (%s ago)\n", t.Local().Format("15:04:05 2006-01-02"), ago)
			} else {
				fmt.Printf("Last Response: %s\n", member.LastResponse)
			}
			if member.Latency != "" {
				fmt.Printf("Latency: %s\n", member.Latency)
			}
		}
	}

	// Print group information
	if len(status.Groups) > 0 {
		fmt.Printf("\nFloating IP Groups:\n")
		fmt.Printf("------------------\n")

		// Groups arrive in map order from the daemon; sort by name so the
		// listing is stable between runs
		sort.Slice(status.Groups, func(i, j int) bool {
			return status.Groups[i].Name < status.Groups[j].Name
		})

		// Map node IDs to hostnames so assignments can show and sort by node name
		hostnames := make(map[string]string, len(status.Members))
		for _, member := range status.Members {
			hostnames[member.NodeID] = member.Hostname
		}

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

			// Print assignments sorted by node name
			if len(group.Assignments) > 0 {
				sort.Slice(group.Assignments, func(i, j int) bool {
					ni, nj := hostnames[group.Assignments[i].NodeID], hostnames[group.Assignments[j].NodeID]
					if ni != nj {
						return ni < nj
					}
					return group.Assignments[i].Interface < group.Assignments[j].Interface
				})
				fmt.Printf("  Assigned to:\n")
				for _, assignment := range group.Assignments {
					name := hostnames[assignment.NodeID]
					if name == "" {
						name = assignment.NodeID
					}
					fmt.Printf("    - Node: %s (%s), Interface: %s\n", name, assignment.NodeID, assignment.Interface)
				}
			} else {
				fmt.Printf("  Assigned to: None\n")
			}
		}
	}

	return nil
}

// witnessLine is what status says about a member's part in arbitration, or ""
// when there is nothing to say -- a cluster with no Witness reads exactly as it
// always has.
//
// The Witness gets one of three readings, because they behave differently and an
// operator diagnosing a partition needs to know which: arbitrating, configured but
// not trusted (it arbitrates nothing until every leg is verified), or down. A
// member with no witness_address in a cluster that has a Witness is named too:
// the Witness reaches it over the heartbeat path, and cannot settle the
// cluster-link-only partition it exists for (ADR-0006).
func witnessLine(m client.Member, all []client.Member) string {
	if m.Witness {
		switch {
		case m.Status == "Unknown":
			return "Witness: down -- a partition leaves both nodes Active until it is back"
		case !m.WitnessTrusted:
			return "Witness: configured but not trusted -- every member needs a published certificate " +
				"and a witness_address (or tls_mode=required); it arbitrates nothing until then"
		default:
			return "Witness: arbitrating"
		}
	}
	for _, other := range all {
		if other.Witness && m.WitnessAddress == "" {
			return "Witness address: none -- reached over the heartbeat path, so a cluster-link-only " +
				"partition is not arbitrated (pulsectl config set witness_address host:port)"
		}
	}
	return ""
}
