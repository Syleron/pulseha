package cli

import (
	"context"
	"errors"
	"fmt"
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
	cmd := &cobra.Command{
		Use:   "demote",
		Short: "Demote a node to passive state",
		RunE: func(cmd *cobra.Command, args []string) error {
			// TODO: Implement node demotion
			return fmt.Errorf("node demotion not implemented yet")
		},
	}
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

	cmd := &cobra.Command{
		Use:   "maintenance",
		Short: "Enter or exit maintenance mode on the local node",
		Long: `Put the local node into maintenance mode so it is excluded from failover elections.
If the node is currently active, a failover is triggered first.
Use --disable to return the node to passive and make it eligible for promotion again.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client.New()
			if err != nil {
				return err
			}
			defer c.Close()

			enable := !disable
			resp, err := c.CLI().SetMaintenance(context.Background(), &rpc.SetMaintenanceRequest{
				Enable: enable,
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
	return cmd
}
