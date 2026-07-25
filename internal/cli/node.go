package cli

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/spf13/cobra"
	"github.com/syleron/pulseha/internal/client"
	rpc "github.com/syleron/pulseha/rpc"
)

func NewNodeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "node",
		Short: "Manage cluster nodes",
		Long:  `Perform operations on cluster nodes such as promoting, demoting, or removing nodes`,
	}

	cmd.AddCommand(
		newNodePromoteCmd(),
		newNodeDemoteCmd(),
		newNodeRemoveCmd(),
		newNodeMaintenanceCmd(),
		newNodeCapacityCmd(),
	)

	return cmd
}

func newNodePromoteCmd() *cobra.Command {
	var nodeID string
	var ips []string
	var force bool

	cmd := &cobra.Command{
		Use:   "promote",
		Short: "Promote a node to active state",
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client.New()
			if err != nil {
				return err
			}
			defer c.Close()

			if nodeID == "" {
				return fmt.Errorf("--node-id is required")
			}

			resp, err := c.CLI().Promote(context.Background(), &rpc.PromoteRequest{
				NodeId:      nodeID,
				Ips:         ips,
				ForceDemote: force,
			})
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

	cmd.Flags().StringVar(&nodeID, "node-id", "", "Node ID (UUID) of the node to promote")
	cmd.Flags().StringSliceVar(&ips, "ips", []string{}, "IPs to assign in active-active mode")
	cmd.Flags().BoolVar(&force, "force", false, "Force promotion if the previous active cannot be demoted")
	cmd.MarkFlagRequired("node-id")

	return cmd
}

func newNodeDemoteCmd() *cobra.Command {
	var nodeID string

	cmd := &cobra.Command{
		Use:   "demote",
		Short: "Demote a node to passive state",
		Long: `Demote a node to passive, bringing down every floating IP it holds.

In active-passive mode the cluster elects a new active node; in active-active
mode the released IPs are redistributed across the remaining nodes.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if nodeID == "" {
				return fmt.Errorf("--node-id is required")
			}

			c, err := client.New()
			if err != nil {
				return err
			}
			defer c.Close()

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			resp, err := c.Server().MakePassive(ctx, &rpc.MakePassiveRequest{NodeId: nodeID})
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

	cmd.Flags().StringVar(&nodeID, "node-id", "", "Node ID (UUID) of the node to demote (required)")
	cmd.MarkFlagRequired("node-id")

	return cmd
}

func newNodeRemoveCmd() *cobra.Command {
	var nodeID string

	cmd := &cobra.Command{
		Use:   "remove",
		Short: "Remove a node from the cluster",
		Long:  `Remove a node from the cluster with coordinated quorum-based removal across all members`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if nodeID == "" {
				return fmt.Errorf("--node-id is required")
			}

			c, err := client.New()
			if err != nil {
				return err
			}
			defer c.Close()

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			resp, err := c.CLI().Leave(ctx, &rpc.LeaveRequest{NodeId: nodeID})
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

	cmd.Flags().StringVar(&nodeID, "node-id", "", "Node ID (UUID) of the node to remove (required)")
	cmd.MarkFlagRequired("node-id")

	return cmd
}

func newNodeMaintenanceCmd() *cobra.Command {
	var disable bool
	var nodeID string

	cmd := &cobra.Command{
		Use:   "maintenance",
		Short: "Enter or exit maintenance mode on a node",
		Long: `Put a node into maintenance mode so it is excluded from failover elections.
If the node is currently active, a failover is triggered first.
Use --disable to return the node to passive and make it eligible for promotion again.
Omit --node-id to target the local node.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client.New()
			if err != nil {
				return err
			}
			defer c.Close()

			enable := !disable
			resp, err := c.CLI().SetMaintenance(context.Background(), &rpc.SetMaintenanceRequest{
				Enable: enable,
				NodeId: nodeID,
			})
			if err != nil {
				return fmt.Errorf("RPC error: %v", err)
			}
			if !resp.Success {
				return errors.New(resp.Message)
			}
			fmt.Println(resp.Message)
			return nil
		},
	}

	cmd.Flags().BoolVar(&disable, "disable", false, "Exit maintenance mode and return the node to passive")
	cmd.Flags().StringVar(&nodeID, "node-id", "", "Node ID (UUID) of the target node; defaults to local node if omitted")
	return cmd
}

func newNodeCapacityCmd() *cobra.Command {
	var nodeID string

	cmd := &cobra.Command{
		Use:   "capacity <limit>",
		Short: "Set the maximum number of floating IPs a node may host",
		Long: `Set a node's floating IP capacity for active-active distribution.
IP placement and rebalancing will not assign more than this many IPs to the node.
A limit of 0 removes the cap (unlimited).
Omit --node-id to target the local node.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			capacity, err := strconv.Atoi(args[0])
			if err != nil || capacity < 0 {
				return fmt.Errorf("limit must be a non-negative integer, got %q", args[0])
			}

			c, err := client.New()
			if err != nil {
				return err
			}
			defer c.Close()

			resp, err := c.CLI().SetCapacity(context.Background(), &rpc.SetCapacityRequest{
				NodeId:   nodeID,
				Capacity: int32(capacity),
			})
			if err != nil {
				return fmt.Errorf("RPC error: %v", err)
			}
			if !resp.Success {
				return errors.New(resp.Message)
			}
			fmt.Println(resp.Message)
			return nil
		},
	}

	cmd.Flags().StringVar(&nodeID, "node-id", "", "Node ID (UUID) or hostname of the target node; defaults to local node if omitted")
	return cmd
}
