package structures

import "errors"

type Configuration struct {
	Client struct {
		       Port int `json:"port"`
	       } `json:"Client"`
	Cluster map[string]ServerID `json:"Cluster"`
}

type ServerID struct {
	IP   string `json:"ip"`
	Port string `json:"port"`
}

func (c *Configuration) Validate() error {
	// TODO: Configuration validation

	// Quick hack to see if the struct is empty
	if c.Client.Port <= 0 {
		err := errors.New("Currupt Config..")
		return err
	}

	return nil;
}
