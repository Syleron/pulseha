package main

import (
	"log"
	"io"
	"time"
	"os"
)

type Config struct {
	NodeName string

	ReconnectInterval time.Duration
	ReconnectTimeout  time.Duration

	Logger    *log.Logger
	LogOutput io.Writer
}

func DefaultLocalConfig() (*Config) {
	hostname, err := os.Hostname()
	if err != nil {
		panic(err)
	}

	return &Config{
		NodeName:          hostname,
		ReconnectTimeout:  24 * time.Hour,
		ReconnectInterval: 30 * time.Second,
	}
}
