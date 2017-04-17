package structures

import (
	"fmt"
)

type Configuration struct {
	General struct {
			Interval int `json:interval"`
			Retries int `json:"retries"`
			UseAddHealth bool `json:"use_add_health"`
			Role string `json:"role"`
			TLS bool `json:"tls"`
			//ClientPort string `json:"client_port"`
			//ServerPort string `json:"server_port"`
	       } `json:"general"`
	//Cluster map[string]ServerID `json:"cluster"`
	Cluster Cluster `json:"cluster"`
}

type Cluster struct {
	Master ClusterDef `json:"master"`
	Slave ClusterDef `json:"slave"`
}

type ClusterDef struct {
	IP   string `json:"ip"`
	Port string `json:"port"`
	//Health []string `json:"health"`
}

func (c *Configuration) Validate() error {
	// TODO: Configuration validation

	// Quick hack to see if the struct is empty
	//if c.General.ServerPort != "" {
	//	err := errors.New("Currupt Config..")
	//	return err
	//}

	// Are we master or are we slave?
	fmt.Print(c.General.Role)

	return nil;
}
