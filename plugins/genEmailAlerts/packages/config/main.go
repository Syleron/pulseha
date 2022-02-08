package config

type Config struct {
	SmtpHost string
	SmtpPort string
	Username string
	Password string
	Email    string
}

// Validate that our config is of the proper structure and data.
func (c *Config) Validate() error {
	return nil
}

func (c *Config) GenerateDefaultConfig() *Config {
	return &Config{
		SmtpHost: "127.0.0.1",
		SmtpPort: "587",
		Username: "",
		Password: "",
		Email:    "",
	}
}
