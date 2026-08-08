package store

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()

	// A file-backed database in a temp dir rather than :memory:, so the tests
	// exercise the same driver path production uses, including WAL.
	path := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func mustUser(t *testing.T, s *Store, email string) *User {
	t.Helper()

	hash, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatalf("hashing password: %v", err)
	}
	u := &User{Email: email, PasswordHash: hash, Role: RoleUser}
	if err := s.Users.Create(context.Background(), u); err != nil {
		t.Fatalf("creating user: %v", err)
	}
	return u
}

func mustServer(t *testing.T, s *Store, ownerID string, port int) *Server {
	t.Helper()

	srv := &Server{
		OwnerID: ownerID, Name: "test", Core: "paper", Version: "1.21.4",
		RAMMb: 2048, Port: port, Dir: t.TempDir(), EULAAccepted: true,
	}
	if err := s.Servers.Create(context.Background(), srv); err != nil {
		t.Fatalf("creating server: %v", err)
	}
	return srv
}

// --- migrations ---

func TestOpenAppliesMigrations(t *testing.T) {
	s := newTestStore(t)

	tables := []string{"users", "tokens", "servers", "backups", "custom_themes", "audit_log"}
	for _, table := range tables {
		var name string
		err := s.DB().QueryRow(
			`SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&name)
		if err != nil {
			t.Errorf("table %s is missing: %v", table, err)
		}
	}
}

// Reopening must not re-run migrations or lose data.
func TestMigrationsAreIdempotent(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "test.db")

	first, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	u := mustUser(t, first, "a@example.com")
	if err := first.Close(); err != nil {
		t.Fatalf("closing: %v", err)
	}

	second, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	defer func() { _ = second.Close() }()

	got, err := second.Users.GetByID(ctx, u.ID)
	if err != nil {
		t.Fatalf("user did not survive reopening: %v", err)
	}
	if got.Email != "a@example.com" {
		t.Fatalf("email = %q after reopen", got.Email)
	}
}

func TestForeignKeysAreEnforced(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	err := s.Servers.Create(ctx, &Server{
		OwnerID: "no-such-user", Name: "orphan", Core: "paper", Version: "1.21.4",
		RAMMb: 1024, Port: 25565, Dir: t.TempDir(),
	})
	if err == nil {
		t.Fatal("created a server owned by a user that does not exist")
	}
}

// --- users ---

func TestUserCRUD(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	u := mustUser(t, s, "user@example.com")
	if u.ID == "" {
		t.Fatal("Create left the id empty")
	}
	if u.CreatedAt.IsZero() || u.UpdatedAt.IsZero() {
		t.Fatal("Create left timestamps unset")
	}
	if u.Theme != "system" {
		t.Errorf("theme = %q, want the system default", u.Theme)
	}

	got, err := s.Users.GetByID(ctx, u.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.Email != u.Email {
		t.Errorf("email = %q, want %q", got.Email, u.Email)
	}

	got.Role = RoleAdmin
	got.MaxServers = 5
	if err := s.Users.Update(ctx, got); err != nil {
		t.Fatalf("Update: %v", err)
	}
	reread, err := s.Users.GetByID(ctx, u.ID)
	if err != nil {
		t.Fatalf("GetByID after update: %v", err)
	}
	if !reread.IsAdmin() || reread.MaxServers != 5 {
		t.Fatalf("update did not persist: %+v", reread)
	}

	if err := s.Users.Delete(ctx, u.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Users.GetByID(ctx, u.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetByID after delete = %v, want ErrNotFound", err)
	}
}

func TestUserEmailIsUniqueCaseInsensitively(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	mustUser(t, s, "Someone@Example.com")

	err := s.Users.Create(ctx, &User{Email: "someone@example.com", PasswordHash: "x"})
	if !errors.Is(err, ErrEmailTaken) {
		t.Fatalf("Create with a differently-cased duplicate = %v, want ErrEmailTaken", err)
	}
}

func TestUserGetByEmailIsCaseInsensitive(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	u := mustUser(t, s, "Mixed@Case.com")

	got, err := s.Users.GetByEmail(ctx, "mixed@case.com")
	if err != nil {
		t.Fatalf("GetByEmail with different case: %v", err)
	}
	if got.ID != u.ID {
		t.Fatalf("got user %s, want %s", got.ID, u.ID)
	}
}

func TestUserNotFound(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if _, err := s.Users.GetByID(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetByID = %v, want ErrNotFound", err)
	}
	if _, err := s.Users.GetByEmail(ctx, "nope@example.com"); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetByEmail = %v, want ErrNotFound", err)
	}
	if err := s.Users.Delete(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Delete = %v, want ErrNotFound", err)
	}
	if err := s.Users.SetTheme(ctx, "nope", "dark"); !errors.Is(err, ErrNotFound) {
		t.Errorf("SetTheme = %v, want ErrNotFound", err)
	}
}

func TestUserSetTheme(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	u := mustUser(t, s, "theme@example.com")
	if err := s.Users.SetTheme(ctx, u.ID, "midnight"); err != nil {
		t.Fatalf("SetTheme: %v", err)
	}

	got, err := s.Users.GetByID(ctx, u.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.Theme != "midnight" {
		t.Fatalf("theme = %q, want midnight", got.Theme)
	}
}

func TestUserCountAndList(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	n, err := s.Users.Count(ctx)
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if n != 0 {
		t.Fatalf("Count on a fresh store = %d, want 0", n)
	}

	mustUser(t, s, "a@example.com")
	mustUser(t, s, "b@example.com")

	if n, err = s.Users.Count(ctx); err != nil || n != 2 {
		t.Fatalf("Count = %d (%v), want 2", n, err)
	}

	users, err := s.Users.List(ctx, 10)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(users) != 2 {
		t.Fatalf("List returned %d users, want 2", len(users))
	}
}

// --- passwords ---

func TestPasswordHashing(t *testing.T) {
	const plain = "correct horse battery staple"

	hash, err := HashPassword(plain)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if strings.Contains(hash, plain) {
		t.Fatal("the hash contains the plaintext password")
	}
	if err := CheckPassword(hash, plain); err != nil {
		t.Fatalf("CheckPassword with the right password: %v", err)
	}
	if err := CheckPassword(hash, "wrong"); !errors.Is(err, ErrBadPassword) {
		t.Fatalf("CheckPassword with the wrong password = %v, want ErrBadPassword", err)
	}

	// bcrypt salts, so the same password must hash differently every time.
	other, err := HashPassword(plain)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if hash == other {
		t.Fatal("two hashes of the same password are identical, so it is not salted")
	}

	if _, err := HashPassword("   "); err == nil {
		t.Error("HashPassword accepted a blank password")
	}
}

// --- tokens ---

func TestTokenLifecycle(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	u := mustUser(t, s, "token@example.com")

	value, hash, err := GenerateToken()
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	if !strings.HasPrefix(value, TokenPrefix) {
		t.Errorf("token %q lacks the %q prefix", value, TokenPrefix)
	}
	if strings.Contains(hash, value) {
		t.Error("the hash contains the token value")
	}

	tok := &Token{
		UserID: u.ID, Name: "ci", Hash: hash,
		Scopes: []string{"servers:read", "servers:console"}, Kind: TokenKindAPI,
	}
	if err := s.Tokens.Create(ctx, tok); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := s.Tokens.GetByHash(ctx, hash)
	if err != nil {
		t.Fatalf("GetByHash: %v", err)
	}
	if got.UserID != u.ID {
		t.Errorf("user id = %q, want %q", got.UserID, u.ID)
	}
	if len(got.Scopes) != 2 || got.Scopes[0] != "servers:read" {
		t.Errorf("scopes = %v, want them round-tripped", got.Scopes)
	}

	if err := s.Tokens.Delete(ctx, tok.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Tokens.GetByHash(ctx, hash); !errors.Is(err, ErrTokenNotFound) {
		t.Fatalf("GetByHash after delete = %v, want ErrTokenNotFound", err)
	}
}

// The stored token value must not be recoverable: only the hash is persisted.
func TestTokenValueIsNeverStored(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	u := mustUser(t, s, "secret@example.com")

	value, hash, err := GenerateToken()
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	if err := s.Tokens.Create(ctx, &Token{UserID: u.ID, Hash: hash}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	var found int
	err = s.DB().QueryRow(`SELECT count(*) FROM tokens WHERE hash = ?`, value).Scan(&found)
	if err != nil {
		t.Fatalf("querying: %v", err)
	}
	if found != 0 {
		t.Fatal("the raw token value is present in the database")
	}
}

func TestTokenExpiry(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	u := mustUser(t, s, "expiry@example.com")

	past := time.Now().Add(-time.Hour)
	future := time.Now().Add(time.Hour)

	_, expiredHash, _ := GenerateToken()
	_, liveHash, _ := GenerateToken()

	if err := s.Tokens.Create(ctx, &Token{UserID: u.ID, Hash: expiredHash, ExpiresAt: &past}); err != nil {
		t.Fatalf("Create expired: %v", err)
	}
	if err := s.Tokens.Create(ctx, &Token{UserID: u.ID, Hash: liveHash, ExpiresAt: &future}); err != nil {
		t.Fatalf("Create live: %v", err)
	}

	expired, err := s.Tokens.GetByHash(ctx, expiredHash)
	if err != nil {
		t.Fatalf("GetByHash: %v", err)
	}
	if !expired.Expired(time.Now()) {
		t.Error("a token that expired an hour ago does not report itself expired")
	}

	live, err := s.Tokens.GetByHash(ctx, liveHash)
	if err != nil {
		t.Fatalf("GetByHash: %v", err)
	}
	if live.Expired(time.Now()) {
		t.Error("a token valid for another hour reports itself expired")
	}

	n, err := s.Tokens.DeleteExpired(ctx, time.Now())
	if err != nil {
		t.Fatalf("DeleteExpired: %v", err)
	}
	if n != 1 {
		t.Fatalf("DeleteExpired removed %d tokens, want 1", n)
	}
	if _, err := s.Tokens.GetByHash(ctx, liveHash); err != nil {
		t.Fatalf("the live token was removed too: %v", err)
	}
}

// A token with no expiry never expires.
func TestTokenWithoutExpiryNeverExpires(t *testing.T) {
	tok := &Token{}
	if tok.Expired(time.Now().Add(100 * 365 * 24 * time.Hour)) {
		t.Fatal("a token with no expiry reported itself expired")
	}
}

func TestTokensCascadeOnUserDelete(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	u := mustUser(t, s, "cascade@example.com")

	_, hash, _ := GenerateToken()
	if err := s.Tokens.Create(ctx, &Token{UserID: u.ID, Hash: hash}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := s.Users.Delete(ctx, u.ID); err != nil {
		t.Fatalf("Delete user: %v", err)
	}
	if _, err := s.Tokens.GetByHash(ctx, hash); !errors.Is(err, ErrTokenNotFound) {
		t.Fatalf("the token outlived its user: %v", err)
	}
}

func TestTokenTouchLastUsed(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	u := mustUser(t, s, "touch@example.com")

	_, hash, _ := GenerateToken()
	tok := &Token{UserID: u.ID, Hash: hash}
	if err := s.Tokens.Create(ctx, tok); err != nil {
		t.Fatalf("Create: %v", err)
	}

	when := time.Now().UTC().Truncate(time.Second)
	if err := s.Tokens.TouchLastUsed(ctx, tok.ID, when); err != nil {
		t.Fatalf("TouchLastUsed: %v", err)
	}

	got, err := s.Tokens.GetByID(ctx, tok.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.LastUsedAt == nil {
		t.Fatal("last_used_at is still unset")
	}
	if !got.LastUsedAt.Truncate(time.Second).Equal(when) {
		t.Fatalf("last_used_at = %v, want %v", got.LastUsedAt, when)
	}
}

func TestTokenListByUser(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	u := mustUser(t, s, "list@example.com")
	other := mustUser(t, s, "other@example.com")

	for i := 0; i < 3; i++ {
		_, hash, _ := GenerateToken()
		if err := s.Tokens.Create(ctx, &Token{UserID: u.ID, Hash: hash}); err != nil {
			t.Fatalf("Create: %v", err)
		}
	}
	_, otherHash, _ := GenerateToken()
	if err := s.Tokens.Create(ctx, &Token{UserID: other.ID, Hash: otherHash}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	tokens, err := s.Tokens.ListByUser(ctx, u.ID)
	if err != nil {
		t.Fatalf("ListByUser: %v", err)
	}
	if len(tokens) != 3 {
		t.Fatalf("ListByUser returned %d tokens, want 3 (another user's must not leak)", len(tokens))
	}
}

// --- servers ---

func TestServerCRUD(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	u := mustUser(t, s, "srv@example.com")

	srv := mustServer(t, s, u.ID, 25565)
	if srv.Kind != KindServer || srv.Status != "stopped" {
		t.Errorf("defaults not applied: kind=%q status=%q", srv.Kind, srv.Status)
	}

	got, err := s.Servers.GetByID(ctx, srv.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.Port != 25565 || !got.EULAAccepted {
		t.Errorf("round trip lost fields: %+v", got)
	}

	got.Name = "renamed"
	got.RAMMb = 4096
	got.AutoStart = true
	if err := s.Servers.Update(ctx, got); err != nil {
		t.Fatalf("Update: %v", err)
	}
	reread, err := s.Servers.GetByID(ctx, srv.ID)
	if err != nil {
		t.Fatalf("GetByID after update: %v", err)
	}
	if reread.Name != "renamed" || reread.RAMMb != 4096 || !reread.AutoStart {
		t.Fatalf("update did not persist: %+v", reread)
	}

	if err := s.Servers.UpdateStatus(ctx, srv.ID, "running"); err != nil {
		t.Fatalf("UpdateStatus: %v", err)
	}
	reread, _ = s.Servers.GetByID(ctx, srv.ID)
	if reread.Status != "running" {
		t.Fatalf("status = %q, want running", reread.Status)
	}

	if err := s.Servers.Delete(ctx, srv.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Servers.GetByID(ctx, srv.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetByID after delete = %v, want ErrNotFound", err)
	}
}

func TestServerPortIsUnique(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	u := mustUser(t, s, "port@example.com")

	mustServer(t, s, u.ID, 25565)

	err := s.Servers.Create(ctx, &Server{
		OwnerID: u.ID, Name: "second", Core: "paper", Version: "1.21.4",
		RAMMb: 1024, Port: 25565, Dir: t.TempDir(),
	})
	if !errors.Is(err, ErrPortInUse) {
		t.Fatalf("Create on a taken port = %v, want ErrPortInUse", err)
	}
}

func TestServerAllocatePort(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	u := mustUser(t, s, "alloc@example.com")

	port, err := s.Servers.AllocatePort(ctx, 25565, 25570)
	if err != nil {
		t.Fatalf("AllocatePort: %v", err)
	}
	if port != 25565 {
		t.Fatalf("first allocation = %d, want the bottom of the range", port)
	}

	mustServer(t, s, u.ID, 25565)
	mustServer(t, s, u.ID, 25566)

	port, err = s.Servers.AllocatePort(ctx, 25565, 25570)
	if err != nil {
		t.Fatalf("AllocatePort: %v", err)
	}
	if port != 25567 {
		t.Fatalf("allocation with 25565-66 taken = %d, want 25567", port)
	}

	// Fill the range and confirm exhaustion is an error, not a silent zero.
	for p := 25567; p <= 25570; p++ {
		mustServer(t, s, u.ID, p)
	}
	if _, err := s.Servers.AllocatePort(ctx, 25565, 25570); err == nil {
		t.Fatal("AllocatePort succeeded on an exhausted range")
	}

	if _, err := s.Servers.AllocatePort(ctx, 100, 50); err == nil {
		t.Error("AllocatePort accepted an inverted range")
	}
}

func TestServerPortInUse(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	u := mustUser(t, s, "inuse@example.com")
	mustServer(t, s, u.ID, 25565)

	used, err := s.Servers.PortInUse(ctx, 25565)
	if err != nil || !used {
		t.Errorf("PortInUse(25565) = %v (%v), want true", used, err)
	}
	if used, err = s.Servers.PortInUse(ctx, 25999); err != nil || used {
		t.Errorf("PortInUse(25999) = %v (%v), want false", used, err)
	}
}

func TestServerListFilters(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	alice := mustUser(t, s, "alice@example.com")
	bob := mustUser(t, s, "bob@example.com")

	a1 := mustServer(t, s, alice.ID, 25565)
	mustServer(t, s, alice.ID, 25566)
	mustServer(t, s, bob.ID, 25567)

	if err := s.Servers.UpdateStatus(ctx, a1.ID, "running"); err != nil {
		t.Fatalf("UpdateStatus: %v", err)
	}

	owned, err := s.Servers.List(ctx, ServerFilter{OwnerID: alice.ID})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(owned) != 2 {
		t.Fatalf("owner filter returned %d servers, want 2", len(owned))
	}

	running, err := s.Servers.List(ctx, ServerFilter{Status: "running"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(running) != 1 || running[0].ID != a1.ID {
		t.Fatalf("status filter returned %d servers, want just the running one", len(running))
	}

	byCore, err := s.Servers.List(ctx, ServerFilter{Core: "paper"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(byCore) != 3 {
		t.Fatalf("core filter returned %d servers, want 3", len(byCore))
	}

	none, err := s.Servers.List(ctx, ServerFilter{Core: "forge"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(none) != 0 {
		t.Fatalf("filtering on an unused core returned %d servers", len(none))
	}
}

func TestServerOwnerAggregates(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	u := mustUser(t, s, "agg@example.com")

	n, err := s.Servers.CountByOwner(ctx, u.ID)
	if err != nil || n != 0 {
		t.Fatalf("CountByOwner on a fresh user = %d (%v), want 0", n, err)
	}
	ram, err := s.Servers.UsedRAMByOwner(ctx, u.ID)
	if err != nil || ram != 0 {
		t.Fatalf("UsedRAMByOwner on a fresh user = %d (%v), want 0", ram, err)
	}

	mustServer(t, s, u.ID, 25565) // 2048 MB each
	mustServer(t, s, u.ID, 25566)

	if n, err = s.Servers.CountByOwner(ctx, u.ID); err != nil || n != 2 {
		t.Fatalf("CountByOwner = %d (%v), want 2", n, err)
	}
	if ram, err = s.Servers.UsedRAMByOwner(ctx, u.ID); err != nil || ram != 4096 {
		t.Fatalf("UsedRAMByOwner = %d (%v), want 4096", ram, err)
	}
}

func TestServersCascadeOnUserDelete(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	u := mustUser(t, s, "cascade2@example.com")
	srv := mustServer(t, s, u.ID, 25565)

	if err := s.Users.Delete(ctx, u.ID); err != nil {
		t.Fatalf("Delete user: %v", err)
	}
	if _, err := s.Servers.GetByID(ctx, srv.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the server outlived its owner: %v", err)
	}
}

// --- custom themes ---

func TestCustomThemeCRUD(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	u := mustUser(t, s, "themes@example.com")

	theme := &CustomTheme{
		UserID: u.ID, Name: "Purple", Base: "dark",
		Vars: map[string]string{"--accent": "#7c5cff", "--radius": "12px"},
	}
	if err := s.CustomThemes.Create(ctx, theme); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := s.CustomThemes.GetByID(ctx, theme.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.Vars["--accent"] != "#7c5cff" || got.Vars["--radius"] != "12px" {
		t.Fatalf("variables did not round trip: %v", got.Vars)
	}

	got.Name = "Deep Purple"
	got.Vars["--accent"] = "#5c3cff"
	if err := s.CustomThemes.Update(ctx, got); err != nil {
		t.Fatalf("Update: %v", err)
	}
	reread, _ := s.CustomThemes.GetByID(ctx, theme.ID)
	if reread.Name != "Deep Purple" || reread.Vars["--accent"] != "#5c3cff" {
		t.Fatalf("update did not persist: %+v", reread)
	}

	themes, err := s.CustomThemes.ListByUser(ctx, u.ID)
	if err != nil {
		t.Fatalf("ListByUser: %v", err)
	}
	if len(themes) != 1 {
		t.Fatalf("ListByUser returned %d themes, want 1", len(themes))
	}

	if err := s.CustomThemes.Delete(ctx, theme.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.CustomThemes.GetByID(ctx, theme.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetByID after delete = %v, want ErrNotFound", err)
	}
}

func TestCustomThemeEmptyVars(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	u := mustUser(t, s, "emptyvars@example.com")

	theme := &CustomTheme{UserID: u.ID, Name: "Bare"}
	if err := s.CustomThemes.Create(ctx, theme); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if theme.Base != "dark" {
		t.Errorf("base = %q, want the dark default", theme.Base)
	}

	got, err := s.CustomThemes.GetByID(ctx, theme.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if len(got.Vars) != 0 {
		t.Fatalf("vars = %v, want empty", got.Vars)
	}
}

// --- audit ---

func TestAuditLog(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	u := mustUser(t, s, "audit@example.com")

	for _, action := range []string{"server.create", "server.start", "server.delete"} {
		err := s.Audit.Append(ctx, &AuditEntry{
			UserID: u.ID, Action: action, Target: "srv-1", IP: "127.0.0.1",
		})
		if err != nil {
			t.Fatalf("Append: %v", err)
		}
	}

	entries, err := s.Audit.List(ctx, u.ID, 10)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("List returned %d entries, want 3", len(entries))
	}
	// Newest first.
	if entries[0].Action != "server.delete" {
		t.Fatalf("first entry = %q, want the newest (server.delete)", entries[0].Action)
	}

	all, err := s.Audit.List(ctx, "", 10)
	if err != nil {
		t.Fatalf("List without a user filter: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("unfiltered list returned %d entries, want 3", len(all))
	}
}

// --- ids ---

func TestNewIDIsUniqueAndSortable(t *testing.T) {
	seen := make(map[string]struct{})
	prev := ""

	for i := 0; i < 1000; i++ {
		id := NewID()
		if _, dup := seen[id]; dup {
			t.Fatalf("NewID collided after %d ids", i)
		}
		seen[id] = struct{}{}

		if len(id) != 26 {
			t.Fatalf("id %q is %d chars, want the 26 of a ULID", id, len(id))
		}
		// Monotonic entropy means this holds even for ids minted inside the
		// same millisecond — which is the whole point, since listings sort
		// by id.
		if prev != "" && id <= prev {
			t.Fatalf("id %q does not sort after the earlier %q", id, prev)
		}
		prev = id
	}
}
