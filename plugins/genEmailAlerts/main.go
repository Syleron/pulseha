package main

import (
	log "github.com/sirupsen/logrus"
	"github.com/syleron/pulseha/plugins/genEmailAlerts/packages/config"
	"github.com/syleron/pulseha/src/pulseha"
)

type PulseEmailAlerts bool

const PluginName = "genEmailAlerts"
const PluginVersion = 1.0

var (
	DB   *pulseha.Database
	conf config.Config
)

func (e PulseEmailAlerts) Name() string {
	return PluginName

}

func (e PulseEmailAlerts) Version() float64 {
	return PluginVersion
}

func (e PulseEmailAlerts) Run(db *pulseha.Database) error {
	DB = db
	_, err := db.Config.GetPluginConfig(e.Name())
	if err != nil {
		if err := db.Config.SetPluginConfig(e.Name(), conf.GenerateDefaultConfig()); err != nil {
			// TODO: Do something with this error
		}
	}
	// Define our config
	//conf = cfg.(config.Config)
	//fmt.Println(">>>^^^^^^^^^ ", conf.SmtpHost)

	// If not, create our config section in the main config w/ defaults
	// Change me
	return nil
}

func (e PulseEmailAlerts) OnMemberListStatusChange(members []pulseha.Member) {
	// Change me
}

func (e PulseEmailAlerts) OnMemberFailover(member pulseha.Member) {
	log.Debug("genEmailAlerts:OnMemberFailover() Sending failover email alert...")
	//if err := email.SendEmail(
	//	conf.Username,
	//	conf.Password,
	//	conf.Email,
	//	conf.SmtpHost,
	//	conf.SmtpPort,
	//	"A PulseHA failover event has occurred. "+member.Hostname+" is now the active appliance.",
	//); err != nil {
	//	fmt.Println("EmailAlerts ", err)
	//	// TODO: Do something on error
	//}
}

var PluginGeneral PulseEmailAlerts
