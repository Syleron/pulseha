package main

import (
	"log"
	"io"
)

type Config struct {
	Logger *log.Logger
	LogOutput io.Writer
}

func DefaultLocalConfig() (*Config) {
	return &Config{}
}