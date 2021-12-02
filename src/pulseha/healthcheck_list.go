package pulseha

import (
	log "github.com/sirupsen/logrus"
	"sync"
)

type HealthChecks struct {
	// Plugins our array of health check plugins
	Plugins []*Plugin
	// sync.Mutex lock for our object
	sync.Mutex
}


// ProcessHCs send all loaded health checks to calculate a score
func (hcs *HealthChecks) ProcessHCs() bool {
	log.Debug("Running health check scheduler total: ", len(hcs.Plugins))
	score := 0
	// Go through our health checks and make an attempt
	for _, hc := range hcs.Plugins  {
		DB.Logging.Debug("Sending health check: " + hc.Name)
		if err := hc.Plugin.(PluginHC).Send(); err != nil {
			// TODO: Do something on error
			continue
		}
		// Success, add our weight to the score.
		score += int(hc.Plugin.(PluginHC).Weight())
	}
	// Update our member score.
	localMember, err := DB.MemberList.GetLocalMember()
	// Handle any errors
	if err != nil {
		// TODO: handle the errors here.
	}
	// Update our local member score
	localMember.SetScore(score)
	return false
}

// TODO: Store the loaded health checks
// TODO: Routinely send health checks