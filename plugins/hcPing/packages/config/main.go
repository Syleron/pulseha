package config

// TODO: Things to remember. Frequency of pings and number of failed requests limit.

type Config struct {
	// An array of Ping groups
	Groups []Group
	// Health check weight for failover calculations
	Weight int32
	// How many times a failure can occur before considering down.
	Threshold int32
}

type Group struct {
	Name string
	Ips []string
}

// Validate that our config is of the proper structure and data.
func (c *Config) Validate() error {
	return nil
}

func (c *Config) GenerateDefaultConfig() *Config {
	return &Config{
		Groups:    make([]Group, 0),
		Weight:    10,
		Threshold: 2,
	}
}
