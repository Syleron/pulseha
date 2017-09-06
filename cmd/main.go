package main

import (
	"fmt"
	"github.com/mitchellh/cli"
	"io/ioutil"
	"log"
	"os"
)

type Release int

const Version = "0.0.1"
const VersionRelease = Release.String()


const (
	DEVELOPMENT    = iota
	PRODUCTION
	BETA
)

func (r Release) String() string {
	switch r {
	case DEVELOPMENT:
		return "dev"
	case PRODUCTION:
		return "prod"
	case BETA:
		return "beta"
	default:
		return "??"
	}
}

func main() {
	os.Exit(realMain())
}

func realMain() int {
	log.SetOutput(ioutil.Discard)

	args := os.Args[1:]
	for _, arg := range args {
		if arg == "-v" || arg == "--version" {
			newArgs := make([]string, len(args)+1)
			newArgs[0] = "version"
			copy(newArgs[1:], args)
			args = newArgs
			break
		}
	}

	cli := &cli.CLI{
		Args:     args,
		Commands: Commands,
		HelpFunc: cli.BasicHelpFunc("pulseha"),
	}

	exitCode, err := cli.Run()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error executing CLI: %s\n", err.Error())
		return 1
	}

	return exitCode
}