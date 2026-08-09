package panelclient

import "time"

// The power actions a server accepts.
const (
	ActionStart   = "start"
	ActionStop    = "stop"
	ActionRestart = "restart"
	ActionKill    = "kill"
)

// The lifecycle states a server reports.
const (
	StatusCreating = "creating"
	StatusStopped  = "stopped"
	StatusStarting = "starting"
	StatusRunning  = "running"
	StatusStopping = "stopping"
	StatusCrashed  = "crashed"
)

// The states a background task passes through.
const (
	TaskRunning = "running"
	TaskDone    = "done"
	TaskFailed  = "failed"
)

// User is a panel account.
type User struct {
	ID         string    `json:"id"`
	Email      string    `json:"email"`
	Role       string    `json:"role"`
	Theme      string    `json:"theme"`
	Blocked    bool      `json:"blocked"`
	MaxServers int       `json:"max_servers"`
	MaxRAMMb   int       `json:"max_ram_mb"`
	MaxDiskMb  int       `json:"max_disk_mb"`
	CreatedAt  time.Time `json:"created_at"`
}

// Identity is the account behind the current token, with what that token is
// allowed to do.
type Identity struct {
	User
	Scopes []string `json:"scopes"`
}

// HasScope reports whether the token carries a scope.
func (i Identity) HasScope(scope string) bool {
	for _, s := range i.Scopes {
		if s == scope {
			return true
		}
	}
	return false
}

// Session is what a successful login returns.
type Session struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
	User      User      `json:"user"`
}

// Metrics is what a running server is currently using. Absent for a server
// that is not running.
type Metrics struct {
	RAMUsedMb     int     `json:"ram_used_mb"`
	CPUPercent    float64 `json:"cpu_percent"`
	UptimeSeconds int64   `json:"uptime_seconds"`
	// PlayersOnline and PlayersMax are nil when the server did not answer a
	// ping — starting up, or not speaking the protocol yet.
	PlayersOnline *int `json:"players_online"`
	PlayersMax    *int `json:"players_max"`
}

// Server is one Minecraft server as the panel sees it.
type Server struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	Core         string    `json:"core"`
	Version      string    `json:"version"`
	Kind         string    `json:"kind"`
	Status       string    `json:"status"`
	RAMMb        int       `json:"ram_mb"`
	Port         int       `json:"port"`
	JavaArgs     string    `json:"java_args"`
	OwnerID      string    `json:"owner_id"`
	AutoStart    bool      `json:"auto_start"`
	AutoRestart  bool      `json:"auto_restart"`
	EULAAccepted bool      `json:"eula_accepted"`
	CreatedAt    time.Time `json:"created_at"`
	Metrics      *Metrics  `json:"metrics,omitempty"`
}

// IsActive reports whether the server is doing something, which is the
// question a bot asks before offering to start or stop it.
func (s Server) IsActive() bool {
	return s.Status == StatusStarting || s.Status == StatusRunning || s.Status == StatusStopping
}

// Task is a long operation the panel is running in the background.
type Task struct {
	ID        string    `json:"id"`
	Kind      string    `json:"kind"`
	ServerID  string    `json:"server_id,omitempty"`
	Status    string    `json:"status"`
	Progress  int       `json:"progress"`
	Error     string    `json:"error,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Finished reports whether the task has stopped changing.
func (t Task) Finished() bool { return t.Status == TaskDone || t.Status == TaskFailed }

// Page is one page of a list, with the cursor for the next.
type Page[T any] struct {
	Items      []T     `json:"items"`
	NextCursor *string `json:"next_cursor"`
}

// Health is what the unauthenticated health endpoint reports.
type Health struct {
	Status  string `json:"status"`
	Version string `json:"version"`
}

// ConsoleLine is one line of a server's output.
type ConsoleLine struct {
	TS     time.Time `json:"ts"`
	Stream string    `json:"stream"`
	Text   string    `json:"text"`
}

// OnlinePlayers is who is on a server right now.
type OnlinePlayers struct {
	Online int `json:"online"`
	Max    int `json:"max"`
	Items  []struct {
		Name string `json:"name"`
		UUID string `json:"uuid"`
	} `json:"items"`
	// Complete is false when the server reported only a sample of the names,
	// which is what a vanilla server does past a dozen players.
	Complete bool `json:"complete"`
}

// CreateServerRequest describes a server to create.
type CreateServerRequest struct {
	Name    string `json:"name"`
	Core    string `json:"core"`
	Version string `json:"version"`
	RAMMb   int    `json:"ram_mb"`
	Port    int    `json:"port,omitempty"`
	// EULAAccepted has to be true: the panel refuses to accept Mojang's terms
	// on an operator's behalf.
	EULAAccepted bool `json:"eula_accepted"`
}

// UpdateServerRequest changes a server. Nil fields are left alone, which is
// what makes this a patch rather than a replacement.
type UpdateServerRequest struct {
	Name        *string `json:"name,omitempty"`
	RAMMb       *int    `json:"ram_mb,omitempty"`
	Port        *int    `json:"port,omitempty"`
	JavaArgs    *string `json:"java_args,omitempty"`
	AutoStart   *bool   `json:"auto_start,omitempty"`
	AutoRestart *bool   `json:"auto_restart,omitempty"`
}

// ListServersOptions filters the server list. An empty field is not sent.
type ListServersOptions struct {
	// Status returns only servers in that state.
	Status string
	// Core returns only servers running that core, such as "paper".
	Core string
}
