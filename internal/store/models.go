package store

import (
	"crypto/rand"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"
)

// Roles a user can hold.
const (
	RoleAdmin = "admin"
	RoleUser  = "user"
)

// Token kinds. Session tokens come from a web login and expire; API tokens are
// created explicitly and may be long-lived.
const (
	TokenKindSession = "session"
	TokenKindAPI     = "api"
)

// Server kinds, which decide default port, protocol and console behaviour.
const (
	KindServer  = "server"
	KindProxy   = "proxy"
	KindBedrock = "bedrock"
)

// Unlimited marks a user limit as having no ceiling.
const Unlimited = 0

// Repository errors.
var (
	ErrNotFound      = errors.New("not found")
	ErrEmailTaken    = errors.New("email is already registered")
	ErrPortInUse     = errors.New("port is already in use")
	ErrNameTaken     = errors.New("server name is already taken")
	ErrTokenNotFound = errors.New("token not found")
)

// User is a panel account.
type User struct {
	ID           string
	Email        string
	PasswordHash string
	Role         string
	Theme        string
	TOTPSecret   string
	Blocked      bool

	// Limits. Zero means unlimited.
	MaxServers int
	MaxRAMMb   int
	MaxDiskMb  int

	CreatedAt time.Time
	UpdatedAt time.Time
}

// IsAdmin reports whether the user holds the admin role.
func (u *User) IsAdmin() bool { return u != nil && u.Role == RoleAdmin }

// Token is an API or session token. Only its hash is ever stored.
type Token struct {
	ID         string
	UserID     string
	Name       string
	Hash       string
	Scopes     []string
	Kind       string
	ExpiresAt  *time.Time
	LastUsedAt *time.Time
	CreatedAt  time.Time
}

// Expired reports whether the token is past its expiry at the given time.
func (t *Token) Expired(now time.Time) bool {
	return t.ExpiresAt != nil && !now.Before(*t.ExpiresAt)
}

// Server is a managed Minecraft server.
type Server struct {
	ID      string
	OwnerID string
	Name    string
	Core    string
	Version string
	Kind    string
	Status  string

	RAMMb    int
	Port     int
	JavaArgs string
	Dir      string
	JarName  string

	AutoStart    bool
	AutoRestart  bool
	EULAAccepted bool

	// ProxyID is the proxy this server sits behind, empty when it is reached
	// directly. A proxy itself never has one: chaining proxies is a thing
	// people do, but not something the panel arranges.
	ProxyID string

	CreatedAt time.Time
	UpdatedAt time.Time
}

// Backup is an archived copy of a server directory.
type Backup struct {
	ID        string
	ServerID  string
	Note      string
	State     string
	SizeBytes int64
	Path      string
	CreatedAt time.Time
}

// CustomTheme is a user-authored theme: a set of variable overrides on top of
// a built-in base.
type CustomTheme struct {
	ID        string
	UserID    string
	Name      string
	Base      string
	Vars      map[string]string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// AuditEntry records one mutating action.
type AuditEntry struct {
	ID        string
	UserID    string
	Action    string
	Target    string
	IP        string
	Details   string
	CreatedAt time.Time
}

// Monotonic entropy, guarded by a mutex because the generator is stateful and
// not safe for concurrent use.
//
// Plain random entropy would leave ids created within the same millisecond in
// arbitrary order, which quietly breaks every "newest first" listing that
// sorts by id. Monotonic entropy makes ids strictly increasing inside a
// millisecond, so lexicographic order really is creation order.
var (
	entropyMu sync.Mutex
	entropy   = ulid.Monotonic(rand.Reader, 0)
)

// NewID returns a fresh ULID, the id format documented in docs/API.md.
func NewID() string {
	entropyMu.Lock()
	defer entropyMu.Unlock()
	return ulid.MustNew(ulid.Timestamp(time.Now()), entropy).String()
}

// encodeScopes joins scopes for storage. Scopes never contain commas, so a
// separated string is enough and keeps the schema free of a join table.
func encodeScopes(scopes []string) string {
	return strings.Join(scopes, ",")
}

func decodeScopes(raw string) []string {
	if raw == "" {
		return nil
	}
	return strings.Split(raw, ",")
}
