package panelclient

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// --- unauthenticated ---

// Health reports whether the panel is up and which version it runs. Needs no
// token, so a bot can say "the panel is unreachable" rather than "your token
// is wrong" when the panel is simply down.
func (c *Client) Health(ctx context.Context) (Health, error) {
	var out Health
	err := c.do(ctx, http.MethodGet, "/health", nil, nil, &out)
	return out, err
}

// --- accounts ---

// Login exchanges an email and password for a session token.
//
// The token is not stored on the client: a caller that wants to keep using it
// passes it to SetToken, which makes the moment a long-lived credential
// starts being used a visible line rather than a side effect.
func (c *Client) Login(ctx context.Context, email, password string) (Session, error) {
	var out Session
	body := map[string]string{"email": email, "password": password}
	err := c.do(ctx, http.MethodPost, "/auth/login", nil, body, &out)
	return out, err
}

// Me returns the account behind the current token and the token's scopes.
func (c *Client) Me(ctx context.Context) (Identity, error) {
	var out Identity
	err := c.do(ctx, http.MethodGet, "/auth/me", nil, nil, &out)
	return out, err
}

// --- servers ---

// ListServers returns the servers the token can see. A non-admin token sees
// only its owner's.
func (c *Client) ListServers(ctx context.Context, opts ListServersOptions) ([]Server, error) {
	query := url.Values{}
	if opts.Status != "" {
		query.Set("status", opts.Status)
	}
	if opts.Core != "" {
		query.Set("core", opts.Core)
	}

	var page Page[Server]
	if err := c.do(ctx, http.MethodGet, "/servers", query, nil, &page); err != nil {
		return nil, err
	}
	return page.Items, nil
}

// GetServer returns one server, including live metrics when it is running.
func (c *Client) GetServer(ctx context.Context, id string) (Server, error) {
	var out Server
	if err := requireID(id); err != nil {
		return out, err
	}
	err := c.do(ctx, http.MethodGet, "/servers/"+url.PathEscape(id), nil, nil, &out)
	return out, err
}

// FindServer returns the server whose name or id matches, case-insensitively.
//
// Bots are driven by people typing names, not ids. The match has to be
// unambiguous: acting on a guess when two servers share a prefix would start
// the wrong world.
func (c *Client) FindServer(ctx context.Context, nameOrID string) (Server, error) {
	wanted := strings.TrimSpace(nameOrID)
	if wanted == "" {
		return Server{}, fmt.Errorf("%w: no server was named", ErrValidation)
	}

	servers, err := c.ListServers(ctx, ListServersOptions{})
	if err != nil {
		return Server{}, err
	}

	var partial []Server
	for _, s := range servers {
		if s.ID == wanted || strings.EqualFold(s.Name, wanted) {
			return s, nil
		}
		if strings.Contains(strings.ToLower(s.Name), strings.ToLower(wanted)) {
			partial = append(partial, s)
		}
	}

	switch len(partial) {
	case 0:
		return Server{}, fmt.Errorf("%w: no server called %q", ErrNotFound, nameOrID)
	case 1:
		return partial[0], nil
	default:
		// The candidates are carried rather than formatted into a sentence:
		// the caller is a bot that speaks to people in its own language, and
		// a message built here would arrive in the wrong one.
		return Server{}, &AmbiguousError{Query: nameOrID, Matches: partial}
	}
}

// AmbiguousError reports that a name matched more than one server.
type AmbiguousError struct {
	// Query is what was asked for.
	Query string
	// Matches are the servers it could have meant.
	Matches []Server
}

func (e *AmbiguousError) Error() string {
	names := make([]string, 0, len(e.Matches))
	for _, s := range e.Matches {
		names = append(names, s.Name)
	}
	return fmt.Sprintf("panelclient: %q matches several servers: %s",
		e.Query, strings.Join(names, ", "))
}

// Is reports the ambiguity as a validation failure too, so a caller that does
// not care which kind it was still handles it.
func (e *AmbiguousError) Is(target error) bool { return target == ErrValidation }

// Names returns the matching servers' names, for a caller building a message.
func (e *AmbiguousError) Names() []string {
	names := make([]string, 0, len(e.Matches))
	for _, s := range e.Matches {
		names = append(names, s.Name)
	}
	return names
}

// CreateServer creates a server and returns it as stored.
func (c *Client) CreateServer(ctx context.Context, req CreateServerRequest) (Server, error) {
	var out Server
	err := c.do(ctx, http.MethodPost, "/servers", nil, req, &out)
	return out, err
}

// UpdateServer changes the fields that are set and returns the server.
func (c *Client) UpdateServer(ctx context.Context, id string, req UpdateServerRequest) (Server, error) {
	var out Server
	if err := requireID(id); err != nil {
		return out, err
	}
	err := c.do(ctx, http.MethodPatch, "/servers/"+url.PathEscape(id), nil, req, &out)
	return out, err
}

// DeleteServer removes a server and its files.
func (c *Client) DeleteServer(ctx context.Context, id string) error {
	if err := requireID(id); err != nil {
		return err
	}
	return c.do(ctx, http.MethodDelete, "/servers/"+url.PathEscape(id), nil, nil, nil)
}

// --- power ---

// Power asks for a lifecycle change and returns the id of the task doing it.
//
// The panel answers before the work is done, because starting a server can
// take minutes. A caller that needs the outcome passes the id to WaitTask.
func (c *Client) Power(ctx context.Context, id, action string, timeout time.Duration) (string, error) {
	if err := requireID(id); err != nil {
		return "", err
	}
	switch action {
	case ActionStart, ActionStop, ActionRestart, ActionKill:
	default:
		return "", fmt.Errorf("%w: %q is not a power action", ErrValidation, action)
	}

	body := map[string]any{"action": action}
	if timeout > 0 {
		body["timeout_seconds"] = int(timeout.Seconds())
	}

	var out struct {
		TaskID string `json:"task_id"`
	}
	err := c.do(ctx, http.MethodPost, "/servers/"+url.PathEscape(id)+"/power", nil, body, &out)
	return out.TaskID, err
}

// Start brings a server up. Like the three below it, this is Power with the
// action spelled out, which is how the bots' commands read.
func (c *Client) Start(ctx context.Context, id string) (string, error) {
	return c.Power(ctx, id, ActionStart, 0)
}

// Stop asks a server to shut down cleanly, waiting up to timeout before the
// panel gives up. Zero uses the panel's own limit.
func (c *Client) Stop(ctx context.Context, id string, timeout time.Duration) (string, error) {
	return c.Power(ctx, id, ActionStop, timeout)
}

// Restart stops a server and starts it again, and starts a stopped one.
func (c *Client) Restart(ctx context.Context, id string) (string, error) {
	return c.Power(ctx, id, ActionRestart, 0)
}

// Kill ends the process without asking it to save. A last resort.
func (c *Client) Kill(ctx context.Context, id string) (string, error) {
	return c.Power(ctx, id, ActionKill, 0)
}

// --- tasks ---

// GetTask returns one background task.
func (c *Client) GetTask(ctx context.Context, id string) (Task, error) {
	var out Task
	if err := requireID(id); err != nil {
		return out, err
	}
	err := c.do(ctx, http.MethodGet, "/tasks/"+url.PathEscape(id), nil, nil, &out)
	return out, err
}

// TaskPollInterval is how often WaitTask asks. A second is fast enough that a
// chat command feels immediate and slow enough not to be a load generator.
const TaskPollInterval = time.Second

// WaitTask polls until the task finishes, the context is done, or the panel
// stops answering.
//
// A failed task is returned with an error built from its own message: a bot
// that says "could not start: port 25565 is in use" is useful, and one that
// says "task failed" is not.
func (c *Client) WaitTask(ctx context.Context, id string) (Task, error) {
	ticker := time.NewTicker(TaskPollInterval)
	defer ticker.Stop()

	for {
		task, err := c.GetTask(ctx, id)
		if err != nil {
			return Task{}, err
		}
		if task.Finished() {
			if task.Status == TaskFailed {
				message := task.Error
				if message == "" {
					message = "the task failed without saying why"
				}
				return task, fmt.Errorf("panelclient: %s: %s", task.Kind, message)
			}
			return task, nil
		}

		select {
		case <-ctx.Done():
			return task, ctx.Err()
		case <-ticker.C:
		}
	}
}

// --- console ---

// MaxHistoryLines is the most the panel will return in one call.
const MaxHistoryLines = 1000

// ConsoleHistory returns the last lines a server printed. Zero lines means
// the panel's default.
func (c *Client) ConsoleHistory(ctx context.Context, id string, lines int) ([]ConsoleLine, error) {
	if err := requireID(id); err != nil {
		return nil, err
	}
	if lines < 0 || lines > MaxHistoryLines {
		return nil, fmt.Errorf("%w: lines must be between 0 and %d", ErrValidation, MaxHistoryLines)
	}

	query := url.Values{}
	if lines > 0 {
		query.Set("lines", strconv.Itoa(lines))
	}

	var page Page[ConsoleLine]
	err := c.do(ctx, http.MethodGet, "/servers/"+url.PathEscape(id)+"/console/history", query, nil, &page)
	return page.Items, err
}

// SendCommand runs one command on a running server.
func (c *Client) SendCommand(ctx context.Context, id, command string) error {
	if err := requireID(id); err != nil {
		return err
	}
	if strings.TrimSpace(command) == "" {
		return fmt.Errorf("%w: the command is empty", ErrValidation)
	}
	return c.do(ctx, http.MethodPost, "/servers/"+url.PathEscape(id)+"/command",
		nil, map[string]string{"command": command}, nil)
}

// --- players ---

// OnlinePlayers asks the server itself who is on it. Fails with ErrNotRunning
// for a server that is not up.
func (c *Client) OnlinePlayers(ctx context.Context, id string) (OnlinePlayers, error) {
	var out OnlinePlayers
	if err := requireID(id); err != nil {
		return out, err
	}
	err := c.do(ctx, http.MethodGet, "/servers/"+url.PathEscape(id)+"/players", nil, nil, &out)
	return out, err
}

// KickPlayer removes a player from a running server. The reason may be empty.
func (c *Client) KickPlayer(ctx context.Context, id, name, reason string) error {
	return c.playerAction(ctx, http.MethodPost, id, name, "kick", reason)
}

// BanPlayer bans a player. The ban outlives a restart.
func (c *Client) BanPlayer(ctx context.Context, id, name, reason string) error {
	return c.playerAction(ctx, http.MethodPost, id, name, "ban", reason)
}

// UnbanPlayer lifts a ban.
func (c *Client) UnbanPlayer(ctx context.Context, id, name string) error {
	return c.playerAction(ctx, http.MethodDelete, id, name, "ban", "")
}

func (c *Client) playerAction(ctx context.Context, method, id, name, action, reason string) error {
	if err := requireID(id); err != nil {
		return err
	}
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("%w: no player was named", ErrValidation)
	}

	var body any
	if reason != "" {
		body = map[string]string{"reason": reason}
	}
	path := "/servers/" + url.PathEscape(id) + "/players/" + url.PathEscape(name) + "/" + action
	return c.do(ctx, method, path, nil, body, nil)
}

// requireID rejects an empty id here rather than sending a request that would
// hit a different route and come back as something unrelated.
func requireID(id string) error {
	if strings.TrimSpace(id) == "" {
		return errors.New("panelclient: the server id is empty")
	}
	return nil
}
