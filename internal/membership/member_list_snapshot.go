package membership

// MembersSnapshot returns a shallow copy of the current member map while holding
// the read lock, allowing callers to iterate safely without racing mutators.
func (m *MemberList) MembersSnapshot() map[string]*Member {
	m.RLock()
	defer m.RUnlock()

	snapshot := make(map[string]*Member, len(m.Members))
	for id, member := range m.Members {
		snapshot[id] = member
	}
	return snapshot
}

// CandidatesSnapshot is MembersSnapshot without the Witness: the members that can
// serve, coordinate or be chosen for anything. Every election, coordinator and
// placement path reads this one, so none of them can see a Witness in any
// status -- up, or down and reading StatusUnknown, which several of them would
// otherwise consider (ADR-0006).
//
// MembersSnapshot remains for whatever has to see every member: the health
// checker that watches the Witness, the status report, the state broadcast.
func (m *MemberList) CandidatesSnapshot() map[string]*Member {
	m.RLock()
	defer m.RUnlock()
	return m.candidatesLocked()
}

// candidatesLocked is CandidatesSnapshot for callers already holding the list
// lock. It takes each member's own lock briefly to read the role, the same order
// getAvailableNodes has always taken them in.
func (m *MemberList) candidatesLocked() map[string]*Member {
	snapshot := make(map[string]*Member, len(m.Members))
	for id, member := range m.Members {
		if member == nil || member.IsWitness() {
			continue
		}
		snapshot[id] = member
	}
	return snapshot
}

// DropClients closes each member's cached client so the next use dials afresh.
//
// Distinct from Member.Close, which is for a member being permanently removed and
// is documented as such. This is for a member that is still in the cluster but
// whose connection was made on terms that no longer apply -- the cluster moving
// between plaintext and TLS (#111). The client is nil afterwards, and
// initializeClient makes a new one on the next call.
func (m *MemberList) DropClients() {
	for _, member := range m.MembersSnapshot() {
		if member != nil {
			member.dropClient()
		}
	}
}
