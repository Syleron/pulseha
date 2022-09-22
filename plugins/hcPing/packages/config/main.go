package config

// TODO: Things to remember. Frequency of pings and number of failed requests limit.

type Config struct {
	// An array of Ping groups
	Groups []Group `json:"groups"`
	// Health check weight for failover calculations
	Weight int32 `json:"weight"`
	// How many address failures can occur before considering down.
	Threshold int32 `json:"threshold"`
	// Maximum number of icmp failures per address
	FailureCount int32 `json:"failureCount"`
}

type Group struct {
	Name string `json:"name"`
	Ips []string `json:"ips"`
}

// Validate that our config is of the proper structure and data.
func (c *Config) Validate() error {
	return nil
}

func (c *Config) GenerateDefaultConfig() *Config {
	return &Config{
		Groups:    make([]Group, 0),
		Weight:    10,
		Threshold: 1,
		FailureCount: 1,
	}
}
