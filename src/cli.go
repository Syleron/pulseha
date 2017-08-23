package main

import (
	"gopkg.in/urfave/cli.v1"
	"os"
	"github.com/Syleron/Pulse/src/utils"
	"fmt"
)

func setupCLI() {
	// Setup the cli
	app := cli.NewApp()

	// List command used to show all the items in cluster
	app.Name = "list"
	app.Usage = "list"
	app.Action = listCluster()

	app.Run(os.Args)

}

func listCluster() error {
	config := utils.LoadConfig()
	config.Validate()
	fmt.Println("Listing the status for all defined nodes within the cluster:\n")
	fmt.Println("Node                       Status")
	fmt.Println("--------                   ------")

	// length of ip is 15

	//var nodes []structures.Nodes
	//
	//nodes = config.Cluster.Nodes
	//
	//for _, server := range nodes {
	//	var spacing = ""
	//
	//	for i := 0; i <= (26 - len(server.Master.IP)); i++ {
	//		spacing += " "
	//	}
	//
	//	fmt.Println(server.Master.IP+spacing+"\x1b[31;1mOffline\x1b[0m")
	//}

	fmt.Println("")

	return nil
}