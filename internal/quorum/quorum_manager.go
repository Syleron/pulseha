package quorum

import (
	"fmt"
	"slices"
	"sync"
	"time"

	log "github.com/charmbracelet/log"
	"github.com/google/uuid"
	"github.com/syleron/pulseha/packages/config"
	"github.com/syleron/pulseha/packages/pulselock"
)

// Constants for session history management
const (
	maxHistorySize = 100            // Maximum number of sessions to keep in history
	historyTTL     = 24 * time.Hour // Sessions older than this are removed
)

// VoteType represents the type of vote being cast
type VoteType string

const (
	// VoteTypeNodeStatus is used for voting on node status changes
	VoteTypeNodeStatus VoteType = "node_status"
	// VoteTypeIPRedistribution is used for voting on IP redistribution
	VoteTypeIPRedistribution VoteType = "ip_redistribution"
	// VoteTypeConfigChange is used for voting on configuration changes
	VoteTypeConfigChange VoteType = "config_change"
)

// VoteDecision represents a vote decision
type VoteDecision string

const (
	// VoteDecisionYes represents a yes vote
	VoteDecisionYes VoteDecision = "yes"
	// VoteDecisionNo represents a no vote
	VoteDecisionNo VoteDecision = "no"
	// VoteDecisionAbstain represents an abstention
	VoteDecisionAbstain VoteDecision = "abstain"
)

// Vote represents a single vote cast by a node
type Vote struct {
	VoterID   string       // ID of the node casting the vote
	Decision  VoteDecision // The vote decision
	Timestamp time.Time    // When the vote was cast
}

// VotingSession represents an active or completed voting session
type VotingSession struct {
	Epoch       int64                // Bound on the first broadcast; never changes on retry.
	MemberIDs   []string             // Fixed electorate for this session.
	ID          string               // Unique ID for this voting session
	Type        VoteType             // Type of vote
	Subject     string               // What is being voted on (node ID, IP, etc.)
	Description string               // Human-readable description
	StartTime   time.Time            // When the voting started
	EndTime     time.Time            // When the voting will end
	Votes       map[string]Vote      // Map of node ID to vote
	Result      *VotingSessionResult // Result of the vote, nil if not completed
}

// Copy returns a deep copy of a VotingSession.
func (s *VotingSession) Copy() *VotingSession {
	// Obtain a struct copy via dereferencing.
	sessionCopy := *s
	sessionCopy.MemberIDs = slices.Clone(s.MemberIDs)
	if s.Result != nil {
		// Deep copy the Result as this is a pointer type.
		sessionCopy.Result = s.Result.Copy()
	}
	// Deep copy the Votes map to prevent chaining of the Vote objects.
	if s.Votes != nil {
		votesCopy := make(map[string]Vote, len(s.Votes))
		for k, v := range s.Votes {
			votesCopy[k] = v
		}
		sessionCopy.Votes = votesCopy
	}
	return &sessionCopy
}

// VotingSessionResult represents the result of a completed voting session.
type VotingSessionResult struct {
	Passed      bool      // Whether the vote passed
	YesCount    int       // Number of yes votes
	NoCount     int       // Number of no votes
	TotalVotes  int       // Total number of votes cast
	QuorumMet   bool      // Whether quorum was met
	CompletedAt time.Time // When the voting completed
}

// Copy returns a deep copy of a VotingSessionResult.
func (s *VotingSessionResult) Copy() *VotingSessionResult {
	// Obtain a struct copy via dereferencing.
	value := *s
	return &value
}

// CompactSessionHistory stores minimal session data for history (uses ~16 bytes vs ~1KB)
type CompactSessionHistory struct {
	Type        uint8   // VoteType as uint8 (0=NodeStatus, 1=IPRedistribution, 2=ConfigChange)
	Passed      uint8   // 0=failed, 1=passed, 2=no_quorum
	YesCount    uint8   // Number of yes votes
	NoCount     uint8   // Number of no votes
	TotalVotes  uint8   // Total votes cast
	CompletedAt uint32  // Unix timestamp (4 bytes vs 24 bytes for time.Time)
	_           [6]byte // Padding to align to 16 bytes
}

// QuorumManager handles quorum-based voting for cluster decisions
type QuorumManager struct {
	pulselock.RWMutex
	configSource   func() *config.Config
	logger         *log.Logger
	activeSessions map[string]*VotingSession
	sessionHistory map[string]*VotingSession // Keep recent full sessions for debugging
	compactHistory []CompactSessionHistory   // Efficient long-term history
	nodeCount      int                       // Total number of nodes in the cluster
	loopWg         sync.WaitGroup
	stopChan       chan struct{}
	loopRunning    bool
}

// NewQuorumManager creates a new quorum manager instance
func NewQuorumManager(cfg *config.Config, logger *log.Logger) *QuorumManager {
	return NewQuorumManagerWithSource(func() *config.Config { return cfg }, logger)
}

// NewQuorumManagerWithSource reads the current config when a session starts.
func NewQuorumManagerWithSource(source func() *config.Config, logger *log.Logger) *QuorumManager {
	cfg := source()
	return &QuorumManager{
		configSource:   source,
		logger:         logger,
		activeSessions: make(map[string]*VotingSession),
		sessionHistory: make(map[string]*VotingSession),
		compactHistory: make([]CompactSessionHistory, 0, maxHistorySize),
		nodeCount:      len(cfg.Nodes),
		stopChan:       make(chan struct{}),
	}
}

// UpdateNodeCount updates the total number of nodes in the cluster
// and automatically adjusts quorum settings based on node count
func (q *QuorumManager) UpdateNodeCount(count int) {
	q.Lock()
	defer q.Unlock()

	q.nodeCount = count
	// Quorum behavior is automatic now; no config toggles to persist
}

// StartVotingSession creates a new voting session and returns its ID
func (q *QuorumManager) StartVotingSession(voteType VoteType, subject string, description string,
	timeout time.Duration) (string, error) {
	if timeout <= 0 {
		return "", fmt.Errorf("voting timeout must be positive")
	}
	switch voteType {
	case VoteTypeNodeStatus, VoteTypeIPRedistribution, VoteTypeConfigChange:
	default:
		return "", fmt.Errorf("invalid vote type")
	}
	cfg := q.configSource()
	if cfg == nil {
		return "", fmt.Errorf("no cluster configuration")
	}
	cfg.Lock()
	members := make([]string, 0, len(cfg.Nodes))
	for id, node := range cfg.Nodes {
		if id != "" && node != nil {
			members = append(members, id)
		}
	}
	cfg.Unlock()
	slices.Sort(members)
	q.Lock()
	defer q.Unlock()

	// Require at least 3 nodes to start a voting session
	if len(members) < 3 {
		return "", fmt.Errorf("quorum voting requires at least 3 nodes")
	}

	// Generate a unique session ID
	sessionID := uuid.New().String()

	// Create the voting session
	session := &VotingSession{
		MemberIDs:   members,
		ID:          sessionID,
		Type:        voteType,
		Subject:     subject,
		Description: description,
		StartTime:   time.Now(),
		EndTime:     time.Now().Add(timeout),
		Votes:       make(map[string]Vote),
	}

	// Add to active sessions
	q.activeSessions[sessionID] = session

	q.logger.Infof("Started voting session %s for %s: %s", sessionID, voteType, description)
	return sessionID, nil
}

// RecoverProposal records the value carried forward by a prepare majority.
// It must happen before any accept votes are counted. Callers must consume this
// subject, not assume their original request was chosen.
func (q *QuorumManager) RecoverProposal(id, subject string) error {
	q.Lock()
	defer q.Unlock()
	session := q.activeSessions[id]
	if session == nil || !time.Now().Before(session.EndTime) || len(session.Votes) != 0 || subject == "" {
		return fmt.Errorf("session cannot recover a proposal")
	}
	session.Subject = subject
	return nil
}

// BindSessionEpoch prevents retries from combining ballots for different epochs.
func (q *QuorumManager) BindSessionEpoch(sessionID string, epoch int64) error {
	q.Lock()
	defer q.Unlock()
	session := q.activeSessions[sessionID]
	if session == nil || !time.Now().Before(session.EndTime) {
		return fmt.Errorf("voting session is not active")
	}
	if epoch <= 0 {
		return fmt.Errorf("invalid proposal epoch")
	}
	if session.Epoch != 0 && session.Epoch != epoch {
		return fmt.Errorf("voting session belongs to a different epoch")
	}
	session.Epoch = epoch
	return nil
}

// CastVote records a vote for a specific voting session
func (q *QuorumManager) CastVote(sessionID string, voterID string, decision VoteDecision) error {
	q.Lock()
	defer q.Unlock()

	// Find the voting session
	session, exists := q.activeSessions[sessionID]
	if !exists {
		// Check if it's in the history
		session, exists = q.sessionHistory[sessionID]
		if !exists {
			return fmt.Errorf("voting session %s not found", sessionID)
		}
		return fmt.Errorf("voting session %s has already concluded", sessionID)
	}

	if !time.Now().Before(session.EndTime) {
		q.concludeVotingSessionLocked(sessionID)
		return fmt.Errorf("voting session %s has expired", sessionID)
	}
	if !slices.Contains(session.MemberIDs, voterID) {
		return fmt.Errorf("voter %s is not in the session electorate", voterID)
	}
	switch decision {
	case VoteDecisionYes, VoteDecisionNo, VoteDecisionAbstain:
	default:
		return fmt.Errorf("invalid vote decision")
	}
	if previous, ok := session.Votes[voterID]; ok {
		if previous.Decision == decision {
			return nil
		}
		return fmt.Errorf("voter %s already voted", voterID)
	}
	// Record the vote
	session.Votes[voterID] = Vote{
		VoterID:   voterID,
		Decision:  decision,
		Timestamp: time.Now(),
	}

	q.logger.Debugf("Recorded vote from %s for session %s: %s", voterID, sessionID, decision)

	// Check if we can conclude the voting
	if q.canConcludeVoting(session) {
		q.concludeVotingSessionLocked(sessionID)
	}

	return nil
}

// GetVotingSession returns information about a specific voting session
func (q *QuorumManager) GetVotingSession(sessionID string) (*VotingSession, error) {
	q.RLock()
	defer q.RUnlock()

	// Check active sessions
	session, exists := q.activeSessions[sessionID]
	if exists {
		// Return a deep copy of the session to protect our mutex.
		return session.Copy(), nil
	}

	// Check session history
	session, exists = q.sessionHistory[sessionID]
	if exists {
		// Return a deep copy of the session to protect our mutex.
		return session.Copy(), nil
	}

	return nil, fmt.Errorf("voting session %s not found", sessionID)
}

// GetActiveVotingSessions returns a list of all active voting sessions
func (q *QuorumManager) GetActiveVotingSessions() []*VotingSession {
	q.RLock()
	defer q.RUnlock()

	sessions := make([]*VotingSession, 0, len(q.activeSessions))
	for _, session := range q.activeSessions {
		// Append a copy of the session to protect our mutex.
		sessions = append(sessions, session.Copy())
	}

	return sessions
}

// ProcessExpiredSessions checks for and concludes any expired voting sessions
func (q *QuorumManager) ProcessExpiredSessions() {
	q.Lock()
	defer q.Unlock()

	now := time.Now()
	for sessionID, session := range q.activeSessions {
		if now.After(session.EndTime) {
			q.logger.Infof("Voting session %s has expired, concluding", sessionID)
			q.concludeVotingSessionLocked(sessionID)
		}
	}
}

// HasQuorum determines if the given vote count meets quorum requirements
func (q *QuorumManager) HasQuorum(voteCount int) bool {
	q.RLock()
	defer q.RUnlock()

	return q.hasQuorumLocked(voteCount)
}

// hasQuorumLocked is the internal implementation of HasQuorum that assumes the
// lock is already held. Internal callers that hold the write lock (CastVote,
// concludeVotingSessionLocked) must use this — calling HasQuorum from them
// would RLock the same RWMutex and self-deadlock.
func (q *QuorumManager) hasQuorumLocked(voteCount int) bool {
	// With fewer than 3 nodes, quorum logic is not applicable
	if q.nodeCount < 3 {
		return true
	}

	// Majority of current node count
	minVotes := (q.nodeCount / 2) + 1
	return voteCount >= minVotes
}

// canConcludeVoting checks if a voting session can be concluded early
// This happens if:
// 1. All nodes have voted, or
// 2. Enough YES votes to pass, or
// 3. Enough NO votes to fail
func (q *QuorumManager) canConcludeVoting(session *VotingSession) bool {
	// If all nodes have voted, we can conclude
	if len(session.Votes) >= len(session.MemberIDs) {
		return true
	}

	// Count votes
	yesCount := 0
	noCount := 0
	for _, vote := range session.Votes {
		switch vote.Decision {
		case VoteDecisionYes:
			yesCount++
		case VoteDecisionNo:
			noCount++
		}
	}

	// If we have enough YES votes to guarantee passage
	if yesCount >= len(session.MemberIDs)/2+1 {
		return true
	}

	// If we have enough NO votes to guarantee failure
	remainingPossibleYes := len(session.MemberIDs) - len(session.Votes)
	minVotes := len(session.MemberIDs)/2 + 1
	if yesCount+remainingPossibleYes < minVotes {
		return true
	}

	return false
}

// concludeVotingSession concludes a voting session and computes the result
func (q *QuorumManager) concludeVotingSession(sessionID string) {
	q.Lock()
	defer q.Unlock()
	q.concludeVotingSessionLocked(sessionID)
}

// concludeVotingSessionLocked is the internal implementation of concludeVotingSession
// that assumes the lock is already held
func (q *QuorumManager) concludeVotingSessionLocked(sessionID string) {
	// Find the session
	session, exists := q.activeSessions[sessionID]
	if !exists {
		q.logger.Warnf("Attempted to conclude non-existent voting session %s", sessionID)
		return
	}

	// Count votes
	yesCount := 0
	noCount := 0
	abstainCount := 0

	for _, vote := range session.Votes {
		switch vote.Decision {
		case VoteDecisionYes:
			yesCount++
		case VoteDecisionNo:
			noCount++
		case VoteDecisionAbstain:
			abstainCount++
		}
	}

	totalVotes := len(session.Votes)
	quorumMet := totalVotes >= len(session.MemberIDs)/2+1

	// Determine if the vote passed
	// A vote passes if:
	// 1. Quorum was met, and
	// 2. An affirmative majority of the fixed electorate
	passed := yesCount >= len(session.MemberIDs)/2+1

	// Create the result
	session.Result = &VotingSessionResult{
		Passed:      passed,
		YesCount:    yesCount,
		NoCount:     noCount,
		TotalVotes:  totalVotes,
		QuorumMet:   quorumMet,
		CompletedAt: time.Now(),
	}

	// Move from active to history
	delete(q.activeSessions, sessionID)
	q.sessionHistory[sessionID] = session

	q.logger.Infof("Concluded voting session %s: passed=%v, quorum=%v, yes=%d, no=%d, total=%d",
		sessionID, passed, quorumMet, yesCount, noCount, totalVotes)

	// Store in compact history and manage memory efficiently
	q.storeCompactHistoryLocked(session)
	q.manageHistoryMemoryLocked()
}

// voteTypeToUint8 converts VoteType to uint8 for compact storage
func voteTypeToUint8(vt VoteType) uint8 {
	switch vt {
	case VoteTypeNodeStatus:
		return 0
	case VoteTypeIPRedistribution:
		return 1
	case VoteTypeConfigChange:
		return 2
	default:
		return 0
	}
}

// storeCompactHistoryLocked stores session in compact format
func (q *QuorumManager) storeCompactHistoryLocked(session *VotingSession) {
	if session.Result == nil {
		return
	}

	compact := CompactSessionHistory{
		Type:        voteTypeToUint8(session.Type),
		YesCount:    uint8(session.Result.YesCount),
		NoCount:     uint8(session.Result.NoCount),
		TotalVotes:  uint8(session.Result.TotalVotes),
		CompletedAt: uint32(session.Result.CompletedAt.Unix()),
	}

	// Set result status
	if !session.Result.QuorumMet {
		compact.Passed = 2 // no_quorum
	} else if session.Result.Passed {
		compact.Passed = 1 // passed
	} else {
		compact.Passed = 0 // failed
	}

	q.compactHistory = append(q.compactHistory, compact)
}

// manageHistoryMemoryLocked keeps memory usage reasonable
func (q *QuorumManager) manageHistoryMemoryLocked() {
	// Keep only recent full sessions (for debugging)
	if len(q.sessionHistory) > 10 {
		// Remove oldest entries, keep newest 10
		count := 0
		for sessionID := range q.sessionHistory {
			if count >= len(q.sessionHistory)-10 {
				break
			}
			delete(q.sessionHistory, sessionID)
			count++
		}
	}

	// Limit compact history size (each entry is only ~16 bytes)
	if len(q.compactHistory) > maxHistorySize {
		// Remove oldest entries, keep newest maxHistorySize
		copy(q.compactHistory, q.compactHistory[len(q.compactHistory)-maxHistorySize:])
		q.compactHistory = q.compactHistory[:maxHistorySize]
	}
}

// cleanupHistoryLocked removes old sessions from history to prevent memory leaks
// Caller must hold the write lock
func (q *QuorumManager) cleanupHistoryLocked() {
	now := time.Now()

	// First pass: remove sessions older than TTL
	for sessionID, session := range q.sessionHistory {
		if session.Result != nil && now.Sub(session.Result.CompletedAt) > historyTTL {
			delete(q.sessionHistory, sessionID)
		}
	}

	// Second pass: if still over size limit, remove oldest sessions
	if len(q.sessionHistory) <= maxHistorySize {
		return
	}

	// Collect sessions with completion times for sorting
	type sessionAge struct {
		id          string
		completedAt time.Time
	}

	var sessions []sessionAge
	for id, session := range q.sessionHistory {
		if session.Result != nil {
			sessions = append(sessions, sessionAge{
				id:          id,
				completedAt: session.Result.CompletedAt,
			})
		}
	}

	// Sort by completion time (oldest first)
	for i := 0; i < len(sessions)-1; i++ {
		for j := i + 1; j < len(sessions); j++ {
			if sessions[i].completedAt.After(sessions[j].completedAt) {
				sessions[i], sessions[j] = sessions[j], sessions[i]
			}
		}
	}

	// Remove oldest sessions until we're under the limit
	sessionsToRemove := len(q.sessionHistory) - maxHistorySize
	for i := 0; i < sessionsToRemove && i < len(sessions); i++ {
		delete(q.sessionHistory, sessions[i].id)
	}

	if sessionsToRemove > 0 {
		q.logger.Debugf("Cleaned up %d old voting sessions from history", sessionsToRemove)
	}
}

// Start starts the quorum manager
func (q *QuorumManager) Start() {
	q.Lock()
	defer q.Unlock()

	if q.loopRunning {
		return
	}

	if q.stopChan == nil {
		q.stopChan = make(chan struct{})
	}

	q.loopRunning = true
	stop := q.stopChan
	q.loopWg.Add(1)
	go q.sessionExpiryLoop(stop)
}

// Stop stops the quorum manager
func (q *QuorumManager) Stop() {
	q.Lock()
	if !q.loopRunning {
		q.Unlock()
		return
	}
	stop := q.stopChan
	q.stopChan = nil
	q.loopRunning = false
	q.Unlock()

	close(stop)
	q.loopWg.Wait()
}

// sessionExpiryLoop periodically checks for and concludes expired voting sessions
func (q *QuorumManager) sessionExpiryLoop(stop <-chan struct{}) {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	defer q.loopWg.Done()

	for {
		select {
		case <-ticker.C:
			q.ProcessExpiredSessions()
		case <-stop:
			return
		}
	}
}
