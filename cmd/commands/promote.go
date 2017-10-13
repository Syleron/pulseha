package commands

import (
	"github.com/mitchellh/cli"
	"strings"
	"flag"
	"google.golang.org/grpc"
	"github.com/Syleron/PulseHA/proto"
)

type PromoteCommand struct {
	Ui cli.Ui
}

/**
 *
 */
func (c *PromoteCommand) Help() string {
	helpText := `
Usage: pulseha status [options] ...
Options:
`
	return strings.TrimSpace(helpText)
}

/**
 *
 */
func (c *PromoteCommand) Run(args []string) int {
	cmdFlags := flag.NewFlagSet("status", flag.ContinueOnError)
	cmdFlags.Usage = func() { c.Ui.Output(c.Help()) }

	connection, err := grpc.Dial("127.0.0.1:9443", grpc.WithInsecure())
	if err != nil {
		c.Ui.Error("GRPC client connection error")
		c.Ui.Error(err.Error())
	}
	defer connection.Close()
	client := proto.NewCLIClient(connection)
	c.drawStatusTable(client)

	return 0
}

/**
 *
 */
func (c *PromoteCommand) drawStatusTable(client proto.CLIClient) {

	}

/**
 *
 */
func (c *PromoteCommand) Synopsis() string {
	return "Provides a status overview of your configured cluster"
}
