package main

import  (
	"sync"
	"github.com/coreos/go-log/log"
	"github.com/Syleron/PulseHA/proto"
)
/**
 * Member struct type
 */
type Member struct {
	hostname   string
	status proto.MemberStatus_Status
	Client
	sync.Mutex
}

/*
 Getters and setters for Member which allow us to make them go routine safe
 */

func (m *Member) getHostname()string {
	m.Lock()
	defer m.Unlock()
	return m.hostname
}
func (m *Member) setHostname(hostname string){
	m.Lock()
	defer m.Unlock()
	m.hostname = hostname
}

func (m *Member) getStatus()proto.MemberStatus_Status {
	m.Lock()
	defer m.Unlock()
	return m.status
}

func (m *Member) setStatus(status proto.MemberStatus_Status) {
	m.Lock()
	defer m.Unlock()
	m.status = status
}
func (m *Member) setClient(client Client) {
	m.Client = client
}

/*
	Make the node active (bring up its groups)
 */
func (m *Member) makeActive()bool{
	log.Debugf("Making active %s", m.getHostname())
	//get all groups send
	//r, err := m.SendMakeActive()
	return true
}

/**
	Make the node passive (take down its groups)
 */
func (m *Member) makePassive()bool {
	log.Debugf("Making passive %s", m.getHostname())
	return true
}