package cli

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/spf13/cobra"
	"github.com/syleron/pulseha/internal/client"
	"github.com/syleron/pulseha/packages/config"
	"github.com/syleron/pulseha/rpc"
)

// isLocalInterfaceIP checks if an IP is assigned to a local interface
func isLocalInterfaceIP(ip string) bool {
    if ip == "" {
        return false
    }
    if ip == "127.0.0.1" || ip == "::1" {
        return true
    }
    ifaces, err := net.Interfaces()
    if err != nil {
        return false
    }
    for _, iface := range ifaces {
        addrs, err := iface.Addrs()
        if err != nil {
            continue
        }
        for _, addr := range addrs {
            switch v := addr.(type) {
            case *net.IPNet:
                if v.IP.String() == ip {
                    return true
                }
            case *net.IPAddr:
                if v.IP.String() == ip {
                    return true
                }
            }
        }
    }
    return false
}

func NewClusterCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cluster",
		Short: "Manage cluster operations",
		Long:  `Perform cluster-wide operations such as creating, joining, or leaving a cluster`,
	}

	cmd.AddCommand(
		newClusterCreateCmd(),
		newClusterJoinCmd(),
		newClusterLeaveCmd(),
		newClusterTokenCmd(),
		newClusterModeCmd(),
		newNetworkCmd(),
		newClusterDoctorCmd(),
	)

	return cmd
}

func newClusterCreateCmd() *cobra.Command {
	var (
		bindIP   string
		bindPort string
		mode     string
		nodeID   string
	)

	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create a new cluster",
		RunE:  createCluster,
	}

	cmd.Flags().StringVar(&bindIP, "bind-ip", "", "IP address to bind to")
	cmd.Flags().StringVar(&bindPort, "bind-port", "8080", "Port to bind to")
	cmd.Flags().StringVar(&mode, "mode", "active-passive", "Cluster mode (active-passive or active-active)")
	cmd.Flags().StringVar(&nodeID, "node-id", "", "Custom node ID (UUID) for the first node")
	cmd.MarkFlagRequired("bind-ip")

	return cmd
}

func newClusterJoinCmd() *cobra.Command {
	var (
		address  string
		token    string
		bindIP   string
		bindPort string
		nodeID   string
	)

	cmd := &cobra.Command{
		Use:   "join",
		Short: "Join an existing cluster",
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := client.New()
			if err != nil {
				return err
			}

			return client.JoinClusterWithNodeID(address, token, bindIP, bindPort, nodeID)
		},
	}

	cmd.Flags().StringVar(&address, "address", "", "Address of an existing cluster member (FQDN or IP:port)")
	cmd.Flags().StringVar(&token, "token", "", "Cluster join token")
	cmd.Flags().StringVar(&bindIP, "bind-ip", "", "Local IP address to bind to (optional)")
	cmd.Flags().StringVar(&bindPort, "bind-port", "", "Local port to bind to (optional)")
	cmd.Flags().StringVar(&nodeID, "node-id", "", "Custom node ID (UUID) for this node")
	cmd.MarkFlagRequired("address")
	cmd.MarkFlagRequired("token")

	return cmd
}

func newClusterLeaveCmd() *cobra.Command {
	var address string
	var nodeID string
	cmd := &cobra.Command{
		Use:   "leave",
		Short: "Leave the current cluster",
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client.New()
			if err != nil {
				return err
			}
			if address != "" {
				return c.LeaveClusterVia(address, nodeID)
			}
			return c.LeaveCluster()
		},
	}
	cmd.Flags().StringVar(&address, "address", "", "Address of a cluster member to send leave to (IP:port)")
	cmd.Flags().StringVar(&nodeID, "node-id", "", "Node ID to remove (defaults to local node)")

	return cmd
}

func newClusterTokenCmd() *cobra.Command {
	var regenerate bool

	cmd := &cobra.Command{
		Use:   "token",
		Short: "Display or regenerate cluster join token",
		Long: `Display the current cluster join token or generate a new one.
		
By default, displays the current token. Use --regenerate to create a new token
and sync it across all cluster members.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := client.New()
			if err != nil {
				return fmt.Errorf("failed to connect to PulseHA daemon - ensure the pulseha service is running: %v", err)
			}
			defer client.Close()

			// Call the Token RPC method
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			resp, err := client.CLI().Token(ctx, &rpc.TokenRequest{Regenerate: regenerate})
			if err != nil {
				return fmt.Errorf("failed to get token: %v", err)
			}

			if !resp.Success {
				return fmt.Errorf("token operation failed: %s", resp.Message)
			}

			// Display the result
			if regenerate {
				fmt.Printf("New cluster token generated:\n%s\n\nToken has been synchronized across all cluster members.\n", resp.Token)
			} else {
				fmt.Println(resp.Token)
			}

			return nil
		},
	}

	cmd.Flags().BoolVar(&regenerate, "regenerate", false, "Generate a new cluster token and sync it to all nodes")

	return cmd
}

func newClusterModeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "mode",
		Short: "Manage cluster operation mode",
	}

	cmd.AddCommand(newClusterModeSetCmd())
	return cmd
}

func newClusterModeSetCmd() *cobra.Command {
	var mode string

	cmd := &cobra.Command{
		Use:   "set",
		Short: "Set cluster operation mode",
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := client.New()
			if err != nil {
				return err
			}

			// Validate mode
			switch mode {
			case "active-passive", "active-active":
				// Valid modes
			default:
				return fmt.Errorf("invalid mode %q: must be either 'active-passive' or 'active-active'", mode)
			}

			return client.SetClusterMode(mode)
		},
	}

	cmd.Flags().StringVar(&mode, "mode", "", "Cluster mode (active-passive or active-active)")
	cmd.MarkFlagRequired("mode")

	return cmd
}

func newNetworkCmd() *cobra.Command {
	netCmd := &cobra.Command{
		Use:   "network",
		Short: "Network utilities",
	}
	netCmd.AddCommand(newNetworkResyncCmd())
	return netCmd
}

func newNetworkResyncCmd() *cobra.Command {
	var createGroups bool
	cmd := &cobra.Command{
		Use:   "resync",
		Short: "Resync network interfaces and optional default groups",
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client.New()
			if err != nil {
				return err
			}
			defer c.Close()
			_, err = c.CLI().ResyncNetwork(context.Background(), &rpc.ResyncNetworkRequest{CreateDefaultGroups: createGroups})
			return err
		},
	}
	cmd.Flags().BoolVar(&createGroups, "create-default-groups", false, "Create default groups for new interfaces")
	return cmd
}

// newClusterDoctorCmd adds diagnostics for common misconfigurations
func newClusterDoctorCmd() *cobra.Command {
    cmd := &cobra.Command{
        Use:   "doctor",
        Short: "Diagnose common cluster issues on this node",
        RunE: func(cmd *cobra.Command, args []string) error {
            // Load current config
            cfg := config.New()
            // Check local node entry
            localID, err := cfg.GetLocalNodeUUID()
            if err != nil {
                return fmt.Errorf("no local node configured: %v", err)
            }
            node := cfg.Nodes[localID]
            if node == nil {
                return fmt.Errorf("local node %s not found in config", localID)
            }

            // 1) Local bind-ip is assigned
            if !isLocalInterfaceIP(node.IP) {
                return fmt.Errorf("bind-ip %s is not assigned to any local interface", node.IP)
            }

            // 2) Duplicate bind tuple
            for id, n := range cfg.Nodes {
                if id == localID {
                    continue
                }
                if n.IP == node.IP && n.Port == node.Port {
                    return fmt.Errorf("bind %s:%s conflicts with node %s (%s)", n.IP, n.Port, id, n.Hostname)
                }
            }

            // 3) Peer connectivity basic checks
            // Try to connect TCP to each peer
            for id, n := range cfg.Nodes {
                if id == localID {
                    continue
                }
                addr := net.JoinHostPort(n.IP, n.Port)
                conn, err := net.DialTimeout("tcp", addr, 750*time.Millisecond)
                if err != nil {
                    return fmt.Errorf("cannot reach peer %s at %s: %v", n.Hostname, addr, err)
                }
                _ = conn.Close()
            }

            fmt.Println("Doctor checks passed: local bind IP valid, no conflicts, peers reachable")
            return nil
        },
    }
    return cmd
}

// createCluster creates a new cluster
func createCluster(cmd *cobra.Command, args []string) error {
	// Get bind IP and port
	bindIP, _ := cmd.Flags().GetString("bind-ip")
	bindPort, _ := cmd.Flags().GetString("bind-port")
	mode, _ := cmd.Flags().GetString("mode")
	nodeID, _ := cmd.Flags().GetString("node-id")

	// Validate mode
	if mode != "active-passive" && mode != "active-active" {
		return fmt.Errorf("invalid mode %q: must be either 'active-passive' or 'active-active'", mode)
	}

	// Create client - this will try to connect to localhost:8080 by default
	c, err := client.New()
	if err != nil {
		return fmt.Errorf("failed to connect to PulseHA daemon - ensure the pulseha service is running: %v", err)
	}
	defer c.Close()

	// Send create cluster request
	req := &rpc.CreateClusterRequest{
		BindIp:   bindIP,
		BindPort: bindPort,
		Mode:     mode,
	}
	if nodeID != "" {
		req.NodeId = nodeID
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	resp, err := c.CLI().CreateCluster(ctx, req)
	if err != nil {
		return err
	}

	if !resp.Success {
		return fmt.Errorf(resp.Message)
	}

	fmt.Println("Cluster created successfully!")

	// Display the cluster token if it was returned
	if resp.Token != "" {
		fmt.Println("\nCluster join token:")
		fmt.Println(resp.Token)
		fmt.Println("\nUse this token when joining other nodes to the cluster.")
	}

	return nil
}
