package main

import (
	"github.com/syleron/pulseha/plugins/hcSerial/internal/hcSerial"
	"github.com/syleron/pulseha/plugins/hcSerial/packages/config"
	"github.com/syleron/pulseha/src/pulseha"
)

type PulseHCSerial bool

const PluginName = "PingHC"
const PluginVersion = 1.0

const PluginWeight = 10

var (
	DB     *pulseha.Database
	conf   config.Config
	serial *hcSerial.HcSerial
)

func (e PulseHCSerial) Name() string {
	return PluginName
}

func (e PulseHCSerial) Version() float64 {
	return PluginVersion
}

func (e PulseHCSerial) Weight() int64 {
	return PluginWeight
}

func (e PulseHCSerial) Run(db *pulseha.Database) error {
	// Setup our config
	_, err := db.Config.GetPluginConfig(e.Name())
	if err != nil {
		if err := db.Config.SetPluginConfig(e.Name(), conf.GenerateDefaultConfig()); err != nil {
			// TODO: Do something with this error
		}
	}
	// Define our config
	//conf = cfg.(config.Config)
	//fmt.Println(">>>^^^^^^^^^ ", conf.SmtpHost)

	// Setup our connection
	hcs, err := hcSerial.Open()

	serial = hcs

	if err != nil {
		panic(err)
	}

	// If not, create our config section in the main config w/ defaults
	// Change me
	return nil
}

func (e PulseHCSerial) Send() error {
	serial.Write([]byte("pulseha"))
	return nil
}

var PluginHC PulseHCSerial
