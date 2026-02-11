package session

// SessionService defines the interface for session management.
// Both the local Manager and the remote gRPC proxy implement this interface.
type SessionService interface {
	StartSession(sessionType SessionType, worktreePath, prompt string) (SessionID, error)
	StopSession(id SessionID) error
	SendFollowUp(id SessionID, message string) error
	CompleteSession(id SessionID) error
	DeleteSession(id SessionID) error

	GetSessionInfo(id SessionID) (SessionInfo, bool)
	GetSessionsForWorktree(path string) []SessionInfo
	GetAllSessions() []SessionInfo
	GetSessionOutput(id SessionID) []OutputLine
	CountByStatus() map[SessionStatus]int

	Events() <-chan interface{}

	LoadHistorySessions(worktreeName string) ([]*SessionMeta, error)
	LoadSessionFromHistory(worktreeName string, id SessionID) (*StoredSession, error)

	IsInTmuxMode() bool
	Close()
}
