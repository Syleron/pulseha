package main

/**
 * Memberlist struct type
 */
type Memberlist struct {
	Members []*Member
}

/**
 * Member struct type
 */
type Member struct {
	Name   string
	Client Client
}

/**
 *
 */
func (m *Memberlist) AddMember(hostname string, client Client) {
	newMember := &Member{
		Name: hostname,
		Client:client,
	}

	m.Members = append(m.Members, newMember)
}

/**
 *
 */
func (m *Memberlist) RemoveMemberByName(hostname string) () {
	for i, member := range m.Members {
		if member.Name == hostname {
			m.Members = append(m.Members[:i], m.Members[i+1:]...)
		}
	}
}

/**
 *
 */
func (m *Memberlist) GetMemberByHostname(hostname string) (*Member) {
	for _, member := range m.Members {
		if member.Name == hostname {
			return member
		}
	}
	return nil
}


