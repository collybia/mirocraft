// Package panelclient is a typed Go client for the Mirocraft REST API.
//
// Both bots are clients of the API rather than of the daemon, per the project
// rule that every decision about a server lives in exactly one place. This
// package is where that rule is made cheap to follow: a bot that wants to
// start a server calls Power here instead of reaching for a process.
//
// It covers what the bots need — accounts, servers, power, the console and
// players — rather than all eighty endpoints. Adding one is a method beside
// its neighbours; pretending to cover the rest would mean untested code
// shaped like a contract.
package panelclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// DefaultTimeout bounds a single request. Generous, because a create can wait
// on a download, and short enough that a bot never hangs on a dead panel.
const DefaultTimeout = 30 * time.Second

// UserAgent identifies the bots in the panel's access log.
const UserAgent = "mirocraft-panelclient/1"

// maxErrorBody caps how much of an error response is read. A panel behind a
// misconfigured proxy can answer a JSON request with a megabyte of HTML.
const maxErrorBody = 64 << 10

// Client talks to one panel. Safe for concurrent use.
type Client struct {
	baseURL *url.URL
	token   string
	http    *http.Client
	agent   string
}

// Option configures a client.
type Option func(*Client)

// WithHTTPClient replaces the underlying HTTP client — for a proxy, a custom
// certificate pool, or a test.
func WithHTTPClient(c *http.Client) Option {
	return func(pc *Client) {
		if c != nil {
			pc.http = c
		}
	}
}

// WithUserAgent appends a bot's own identifier to the user agent.
func WithUserAgent(agent string) Option {
	return func(pc *Client) {
		if strings.TrimSpace(agent) != "" {
			pc.agent = UserAgent + " " + agent
		}
	}
}

// New returns a client for the panel at rawURL, authenticating with token.
//
// The address may be given with or without the /api/v1 prefix and with or
// without a trailing slash: an operator pasting the address from the browser
// gets one of those, and refusing it would be pedantry rather than safety.
//
// The token may be empty, for the endpoints that need no authentication and
// for logging in to obtain one.
func New(rawURL, token string, opts ...Option) (*Client, error) {
	trimmed := strings.TrimSpace(rawURL)
	if trimmed == "" {
		return nil, errors.New("panelclient: the panel address is empty")
	}
	if !strings.Contains(trimmed, "://") {
		// A bare host is what an operator types; https rather than http,
		// because sending a token in the clear should take saying so.
		trimmed = "https://" + trimmed
	}

	parsed, err := url.Parse(trimmed)
	if err != nil {
		return nil, fmt.Errorf("panelclient: parsing the panel address: %w", err)
	}
	if parsed.Host == "" {
		return nil, fmt.Errorf("panelclient: %q has no host", rawURL)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("panelclient: %q is not an http address", rawURL)
	}

	parsed.Path = strings.TrimSuffix(parsed.Path, "/")
	parsed.Path = strings.TrimSuffix(parsed.Path, "/api/v1")
	parsed.RawQuery = ""
	parsed.Fragment = ""

	c := &Client{
		baseURL: parsed,
		token:   strings.TrimSpace(token),
		http:    &http.Client{Timeout: DefaultTimeout},
		agent:   UserAgent,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c, nil
}

// BaseURL returns the panel's address without the API prefix.
func (c *Client) BaseURL() string { return c.baseURL.String() }

// Token returns the token in use. Exposed so a bot can persist a session it
// obtained through Login; it is never logged by this package.
func (c *Client) Token() string { return c.token }

// SetToken replaces the token, for a client that logs in after construction.
func (c *Client) SetToken(token string) { c.token = strings.TrimSpace(token) }

// endpoint builds an absolute URL for an API path such as "/servers".
func (c *Client) endpoint(path string, query url.Values) string {
	u := *c.baseURL
	u.Path = strings.TrimSuffix(u.Path, "/") + "/api/v1" + path
	if len(query) > 0 {
		u.RawQuery = query.Encode()
	}
	return u.String()
}

// do performs a request and decodes a JSON response into out, which may be
// nil for endpoints that answer with no body.
func (c *Client) do(ctx context.Context, method, path string, query url.Values, body, out any) error {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("panelclient: encoding the request: %w", err)
		}
		reader = bytes.NewReader(raw)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.endpoint(path, query), reader)
	if err != nil {
		return fmt.Errorf("panelclient: building the request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.agent)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("panelclient: %s %s: %w", method, path, err)
	}
	defer func() {
		// Drained before closing so the connection returns to the pool: a bot
		// polling every few seconds otherwise opens a new one each time.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxErrorBody))
		_ = resp.Body.Close()
	}()

	if resp.StatusCode >= 400 {
		return parseError(resp)
	}
	if out == nil || resp.StatusCode == http.StatusNoContent {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("panelclient: decoding the response to %s %s: %w", method, path, err)
	}
	return nil
}

// --- errors ---

// The conditions a caller acts on differently. Compared with errors.Is; the
// code in the response body is what they are matched on, not the status,
// because the API documents the codes as the stable part.
var (
	// ErrUnauthorized means the token is missing, expired or wrong.
	ErrUnauthorized = errors.New("panelclient: not authenticated")
	// ErrForbidden means the token is valid but lacks the scope, or the
	// object belongs to someone else.
	ErrForbidden = errors.New("panelclient: not allowed")
	// ErrNotFound means the server or object does not exist.
	ErrNotFound = errors.New("panelclient: not found")
	// ErrNotRunning means the operation needs a running server.
	ErrNotRunning = errors.New("panelclient: the server is not running")
	// ErrValidation means the request was rejected as malformed.
	ErrValidation = errors.New("panelclient: the request was rejected")
	// ErrRateLimited means the panel asked the caller to slow down. The wait
	// is on Error.RetryAfter.
	ErrRateLimited = errors.New("panelclient: rate limited")
	// ErrAlreadyRunning means a start was asked for a server that is already
	// up. Distinct from ErrNotRunning so a bot can say which it was.
	ErrAlreadyRunning = errors.New("panelclient: the server is already running")
)

// Error is a rejection from the panel, carrying everything the API documents
// about it.
type Error struct {
	// StatusCode is the HTTP status.
	StatusCode int
	// Code is the machine-readable code from docs/API.md, such as
	// "server_not_running". Empty when the body was not the API's own error
	// shape — a proxy's 502 page, for instance.
	Code string
	// Message is the human-readable explanation.
	Message string
	// Details carries the extra fields some errors add, such as the offending
	// field name on a validation failure.
	Details map[string]any
	// RetryAfter is how long the panel asked the caller to wait. Zero unless
	// the response carried a Retry-After header.
	RetryAfter time.Duration
}

func (e *Error) Error() string {
	switch {
	case e.Code != "" && e.Message != "":
		return fmt.Sprintf("panelclient: %s (%s)", e.Message, e.Code)
	case e.Message != "":
		return "panelclient: " + e.Message
	default:
		return fmt.Sprintf("panelclient: the panel answered %d", e.StatusCode)
	}
}

// Is maps an error onto the sentinels above, so callers can write
// errors.Is(err, panelclient.ErrNotFound) instead of comparing codes.
//
// The status is the fallback rather than the rule: a 404 from a proxy in
// front of the panel carries no code, and treating it as "no such server"
// would be a guess a caller should not act on blindly — but it is still the
// closest true statement available.
func (e *Error) Is(target error) bool {
	switch target {
	case ErrUnauthorized:
		return e.Code == codeUnauthorized || (e.Code == "" && e.StatusCode == http.StatusUnauthorized)
	case ErrForbidden:
		return e.Code == codeForbidden || (e.Code == "" && e.StatusCode == http.StatusForbidden)
	case ErrNotFound:
		return e.Code == codeServerNotFound || (e.Code == "" && e.StatusCode == http.StatusNotFound)
	case ErrNotRunning:
		return e.Code == codeServerNotRunning
	case ErrAlreadyRunning:
		return e.Code == codeServerAlreadyRunning
	case ErrValidation:
		return e.Code == codeValidationFailed || (e.Code == "" && e.StatusCode == http.StatusBadRequest)
	case ErrRateLimited:
		return e.Code == codeRateLimited || (e.Code == "" && e.StatusCode == http.StatusTooManyRequests)
	default:
		return false
	}
}

// The codes the API documents. Duplicated rather than imported: internal/api
// is the server, and a client that imports the server would make the two
// impossible to version separately.
const (
	codeValidationFailed     = "validation_failed"
	codeUnauthorized         = "unauthorized"
	codeForbidden            = "forbidden"
	codeServerNotFound       = "server_not_found"
	codeServerNotRunning     = "server_not_running"
	codeServerAlreadyRunning = "server_already_running"
	codeRateLimited          = "rate_limited"
)

// parseError turns a failed response into an *Error.
func parseError(resp *http.Response) error {
	out := &Error{StatusCode: resp.StatusCode, RetryAfter: retryAfter(resp)}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	if err != nil || len(raw) == 0 {
		return out
	}

	var envelope struct {
		Error struct {
			Code    string         `json:"code"`
			Message string         `json:"message"`
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		// Not the API's shape. The body is not put into the message: it may be
		// a page of HTML, and an error a bot posts into a chat should be a
		// sentence.
		return out
	}
	out.Code = envelope.Error.Code
	out.Message = envelope.Error.Message
	out.Details = envelope.Error.Details
	return out
}

// retryAfter reads the header in both forms RFC 9110 allows.
func retryAfter(resp *http.Response) time.Duration {
	value := resp.Header.Get("Retry-After")
	if value == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(value); err == nil && seconds >= 0 {
		return time.Duration(seconds) * time.Second
	}
	if when, err := http.ParseTime(value); err == nil {
		if wait := time.Until(when); wait > 0 {
			return wait
		}
	}
	return 0
}
