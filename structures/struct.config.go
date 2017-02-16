package structures

import "errors"

type Configuration struct {
	General struct {
			Interval int `json:interval"`
			Retries int `json:"retries"`
			UseAddHealth bool `json:"use_add_health"`
			ClientPort string `json:"client_port"`
			ServerPort string `json:"server_port"`
	       } `json:"general"`
	Cluster map[string]ServerID `json:"cluster"`
}

type ServerID struct {
	IP   string `json:"ip"`
	Port string `json:"port"`
	Health []string `json:"health"`
}

func (c *Configuration) Validate() error {
	// TODO: Configuration validation

	// Quick hack to see if the struct is empty
	if c.General.ServerPort != "" {
		err := errors.New("Currupt Config..")
		return err
	}

	return nil;
}
