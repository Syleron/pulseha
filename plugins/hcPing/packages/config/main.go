package config

// TODO: Things to remember. Frequency of pings and number of failed requests limit.

type Config struct {
	// An array of Ping groups
	Groups map[string][]Group
	// Health check weight for failover calculations
	Weight int32
	// How many times a failure can occur before considering down.
	Threshold int32
}

type Group struct {
	Ips []string
}

// Validate that our config is of the proper structure and data.
func (c *Config) Validate() error {
	return nil
}

func (c *Config) GenerateDefaultConfig() *Config {
	return &Config{
		Groups:    make(map[string][]Group),
		Weight:    10,
		Threshold: 2,
	}
}
