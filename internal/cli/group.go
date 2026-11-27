package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"github.com/syleron/pulseha/internal/client"
)

func NewGroupCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "group",
		Short: "Manage IP groups",
		Long:  `Manage floating IP groups and their assignments`,
	}

	cmd.AddCommand(
		newGroupCreateCmd(),
		newGroupAddIPCmd(),
		newGroupRemoveIPCmd(),
		newGroupAssignCmd(),
		newGroupUnassignCmd(),
		newGroupDeleteCmd(),
		newGroupListCmd(),
	)

	return cmd
}

func newGroupCreateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "create <group-name>",
		Short: "Create a new IP group",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]

			client, err := client.New()
			if err != nil {
				fmt.Printf("Failed to create client: %v\n", err)
				os.Exit(1)
			}

			if err := client.CreateGroup(name); err != nil {
				fmt.Printf("Failed to create group: %v\n", err)
				os.Exit(1)
			}

			return nil
		},
	}

	return cmd
}

func newGroupAddIPCmd() *cobra.Command {
	var (
		group        string
		ip           string
		verifyUnused bool
	)

	cmd := &cobra.Command{
		Use:   "add-ip",
		Short: "Add an IP to a group",
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := client.New()
			if err != nil {
				fmt.Printf("Failed to create client: %v\n", err)
				os.Exit(1)
			}

			if err := client.AddIPToGroup(group, ip, verifyUnused); err != nil {
				fmt.Printf("Failed to add IP to group: %v\n", err)
				os.Exit(1)
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&group, "group", "", "Name of the group to add the IP to")
	cmd.Flags().StringVar(&ip, "ip", "", "IP address to add to the group (with optional subnet mask)")
	cmd.Flags().BoolVar(&verifyUnused, "verify-unused", false,
		"Refuse the address if another host on the segment already answers for it "+
			"(IPv4; not for networks that answer ARP for every address)")
	cmd.MarkFlagRequired("group")
	cmd.MarkFlagRequired("ip")

	return cmd
}

func newGroupRemoveIPCmd() *cobra.Command {
	var (
		group string
		ip    string
	)

	cmd := &cobra.Command{
		Use:   "remove-ip",
		Short: "Remove an IP from a group",
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := client.New()
			if err != nil {
				fmt.Printf("Failed to create client: %v\n", err)
				os.Exit(1)
			}

			if err := client.RemoveIPFromGroup(group, ip); err != nil {
				fmt.Printf("Failed to remove IP from group: %v\n", err)
				os.Exit(1)
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&group, "group", "", "Name of the group to remove the IP from")
	cmd.Flags().StringVar(&ip, "ip", "", "IP address to remove from the group (with optional subnet mask)")
	cmd.MarkFlagRequired("group")
	cmd.MarkFlagRequired("ip")

	return cmd
}

func newGroupAssignCmd() *cobra.Command {
	var (
		group  string
		nodeID string
		iface  string
	)

	cmd := &cobra.Command{
		Use:   "assign",
		Short: "Assign a group to a node's interface",
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := client.New()
			if err != nil {
				fmt.Printf("Failed to create client: %v\n", err)
				os.Exit(1)
			}

			if err := client.AssignGroupToNode(group, nodeID, iface); err != nil {
				fmt.Printf("Failed to assign group to node: %v\n", err)
				os.Exit(1)
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&group, "group", "", "Name of the group to assign")
	cmd.Flags().StringVar(&nodeID, "node-id", "", "Node ID (UUID) of the node to assign the group to")
	cmd.Flags().StringVar(&iface, "interface", "", "Network interface to assign the group to")
	cmd.MarkFlagRequired("group")
	cmd.MarkFlagRequired("node-id")
	cmd.MarkFlagRequired("interface")

	return cmd
}

func newGroupListCmd() *cobra.Command {
	var jsonOutput bool

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List all IP groups",
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := client.New()
			if err != nil {
				fmt.Printf("Failed to create client: %v\n", err)
				os.Exit(1)
			}

			jsonData, groups, err := client.ListGroups(jsonOutput)
			if err != nil {
				fmt.Printf("Failed to list groups: %v\n", err)
				os.Exit(1)
			}

			if jsonOutput {
				if jsonData != "" {
					fmt.Println(jsonData)
				} else {
					fmt.Println("{}")
				}
				return nil
			}

			if len(groups) == 0 {
				fmt.Println("No IP groups configured")
				return nil
			}

			// Pretty print the groups
			for _, group := range groups {
				fmt.Printf("Group: %s\n", group.Name)

				if len(group.Ips) > 0 {
					fmt.Println("  IPs:")
					for _, ip := range group.Ips {
						fmt.Printf("    - %s\n", ip)
					}
				} else {
					fmt.Println("  IPs: None")
				}

				if len(group.Assignments) > 0 {
					fmt.Println("  Assigned to:")
					for _, assignment := range group.Assignments {
						fmt.Printf("    - Node ID: %s, Interface: %s\n", assignment.NodeId, assignment.Interface)
					}
				} else {
					fmt.Println("  Assigned to: None")
				}

				fmt.Println()
			}

			return nil
		},
	}

	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Output in JSON format")

	return cmd
}

func newGroupUnassignCmd() *cobra.Command {
	var (
		group  string
		nodeID string
		iface  string
	)

	cmd := &cobra.Command{
		Use:   "unassign",
		Short: "Unassign a group from a node's interface",
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := client.New()
			if err != nil {
				fmt.Printf("Failed to create client: %v\n", err)
				os.Exit(1)
			}

			if err := client.UnassignGroupFromNode(group, nodeID, iface); err != nil {
				fmt.Printf("Failed to unassign group from node: %v\n", err)
				os.Exit(1)
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&group, "group", "", "Name of the group to unassign")
	cmd.Flags().StringVar(&nodeID, "node-id", "", "Node ID (UUID) of the node to unassign the group from")
	cmd.Flags().StringVar(&iface, "interface", "", "Network interface to unassign the group from")
	cmd.MarkFlagRequired("group")
	cmd.MarkFlagRequired("node-id")
	cmd.MarkFlagRequired("interface")

	return cmd
}

func newGroupDeleteCmd() *cobra.Command {
	var force bool

	cmd := &cobra.Command{
		Use:   "delete <group-name>",
		Short: "Delete an IP group",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]

			client, err := client.New()
			if err != nil {
				fmt.Printf("Failed to create client: %v\n", err)
				os.Exit(1)
			}

			if err := client.DeleteGroup(name, force); err != nil {
				fmt.Printf("Failed to delete group: %v\n", err)
				os.Exit(1)
			}

			return nil
		},
	}

	cmd.Flags().BoolVar(&force, "force", false, "Force deletion even if assigned to nodes")

	return cmd
}
