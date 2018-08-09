package server

import (
	"sync"
	"github.com/Syleron/PulseHA/src/config"
)

var db Database

type Database struct {
	sync.Mutex
	config.Config
}

/**
Returns a copy of the config
*/
func (d *Database) GetConfig() config.Config {
	return d.Config
}

/**
Should this save auto?
*/
func (d *Database) SetConfig(config config.Config) {
	d.Lock()
	d.Config = config
	//set who we are might need to go somewhere else
	d.Unlock()
}

