package membership

import (
	"time"
)

// MemberHealth contains detailed health information about a member
type MemberHealth struct {
	Hostname     string
	Status       MemberStatus
	ActiveIPs    []string
	LastResponse time.Time
	Latency      string
}
