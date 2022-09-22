package config

type Config struct {
	// PortName
	PortName string
	// BaudRate
	BaudRate uint
	// Health check weight for fail-over calculations
	Weight int32
}

// Validate that our config is of the proper structure and data.
func (c *Config) Validate() error {
	return nil
}

// GenerateDefaultconfig
func (c *Config) GenerateDefaultConfig() *Config {
	return &Config{}
}
