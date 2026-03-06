package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"
	"github.com/syleron/pulseha/internal/client"
	"github.com/syleron/pulseha/rpc"
)

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
		newClusterConvergeCmd(),
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
	cmd := &cobra.Command{
		Use:   "leave",
		Short: "Leave the current cluster",
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := client.New()
			if err != nil {
				return err
			}
			defer client.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			resp, err := client.CLI().Leave(ctx, &rpc.LeaveRequest{})
			if err != nil {
				return err
			}
			if !resp.Success {
				return errors.New(resp.Message)
			}
			fmt.Println(resp.Message)
			return nil
		},
	}

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
				return fmt.Errorf("failed to connect to PulseHA daemon - ensure the pulseha service is running: %v",
					err)
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
				fmt.Printf("New cluster token generated:\n%s\n\nToken has been synchronized across all cluster members.\n",
					resp.Token)
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
			c, err := client.New()
			if err != nil {
				return err
			}
			defer c.Close()
			resp, err := c.CLI().SetMode(context.Background(), &rpc.SetModeRequest{Mode: mode})
			if err != nil {
				return err
			}
			if !resp.Success {
				return errors.New(resp.Message)
			}
			fmt.Println(resp.Message)
			return nil
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
			resp, err := c.CLI().ResyncNetwork(context.Background(),
				&rpc.ResyncNetworkRequest{CreateDefaultGroups: createGroups})
			if err != nil {
				return err
			}
			if !resp.Success {
				return errors.New(resp.Message)
			}
			fmt.Println(resp.Message)
			return nil
		},
	}
	cmd.Flags().BoolVar(&createGroups, "create-default-groups", false, "Create default groups for new interfaces")
	return cmd
}

func newClusterConvergeCmd() *cobra.Command {
	var leaderID string
	cmd := &cobra.Command{
		Use:   "converge",
		Short: "Force convergence by broadcasting current cluster state",
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client.New()
			if err != nil {
				return err
			}
			defer c.Close()

			// Pull status to build states map
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			resp, err := c.CLI().Status(ctx, &rpc.StatusRequest{})
			if err != nil {
				return err
			}
			states := make(map[string]int)
			for _, m := range resp.Members {
				states[m.NodeId] = int(m.Status)
			}

			// Encode enhanced payload (states + optional leaderID)
			payload := map[string]interface{}{
				"member_states": states,
			}
			if leaderID != "" {
				payload["leader_id"] = leaderID
			}
			bytes, err := json.Marshal(payload)
			if err != nil {
				return err
			}

			// Call Server.ConfigSync locally (the daemon will broadcast to peers)
			cres, err := c.Server().ConfigSync(context.Background(), &rpc.ConfigSyncRequest{Config: bytes})
			if err != nil {
				return err
			}
			if cres != nil && cres.Message != "" {
				fmt.Println(cres.Message)
			} else {
				fmt.Println("Convergence broadcast requested")
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&leaderID, "leader-id", "", "Override leader ID to enforce in active-passive mode")
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
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	resp, err := c.CLI().CreateCluster(ctx, req)
	if err != nil {
		return err
	}

	if !resp.Success {
		return errors.New(resp.Message)
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
