package panelclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

// The frame types the console socket uses.
const (
	FrameLine    = "line"
	FrameStatus  = "status"
	FrameError   = "error"
	frameCommand = "command"
)

// Frame is one message from the console socket. Exactly one of Line, Status
// or Err is meaningful, chosen by Type.
type Frame struct {
	// Type is one of FrameLine, FrameStatus or FrameError.
	Type string `json:"type"`

	// Line fields, for Type == FrameLine.
	TS     time.Time `json:"ts"`
	Stream string    `json:"stream"`
	Text   string    `json:"text"`

	// Status field, for Type == FrameStatus.
	Status string `json:"status"`

	// Error fields, for Type == FrameError. A rejected command is reported
	// here rather than by the socket closing, so a caller can tell a command
	// that failed from a connection that dropped.
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Ticket is a one-shot credential for opening a socket.
type Ticket struct {
	Ticket    string    `json:"ticket"`
	ExpiresAt time.Time `json:"expires_at"`
}

// ConsoleTicket issues a ticket for a server's console socket.
//
// A separate step because a browser cannot set an Authorization header on a
// WebSocket upgrade, so the panel authenticates the upgrade with a one-shot
// token in the query string instead. A bot inherits the same mechanism.
func (c *Client) ConsoleTicket(ctx context.Context, id string) (Ticket, error) {
	var out Ticket
	if err := requireID(id); err != nil {
		return out, err
	}
	err := c.do(ctx, http.MethodPost, "/servers/"+url.PathEscape(id)+"/console/ticket", nil, nil, &out)
	return out, err
}

// EventsTicket issues a ticket for the panel-wide event socket.
func (c *Client) EventsTicket(ctx context.Context) (Ticket, error) {
	var out Ticket
	err := c.do(ctx, http.MethodPost, "/events/ticket", nil, nil, &out)
	return out, err
}

// Console is an open console connection.
type Console struct {
	conn *websocket.Conn
}

// OpenConsole issues a ticket and opens the console socket with it.
//
// The returned Console must be closed. The scrollback the panel replays on
// connect arrives as ordinary line frames before the live ones, so a caller
// reading in a loop needs no special case for it.
//
// A successful return does not mean the console is live. The panel accepts
// the upgrade first and only then looks for the server, so "that server is
// not running" arrives as an error frame on the first Read rather than as an
// error here. A caller that wants to know before it prints anything reads one
// frame and checks its type.
func (c *Client) OpenConsole(ctx context.Context, id string) (*Console, error) {
	ticket, err := c.ConsoleTicket(ctx, id)
	if err != nil {
		return nil, err
	}
	return c.dial(ctx, "/servers/"+url.PathEscape(id)+"/console", ticket.Ticket)
}

// OpenEvents opens the panel-wide event socket.
func (c *Client) OpenEvents(ctx context.Context) (*Console, error) {
	ticket, err := c.EventsTicket(ctx)
	if err != nil {
		return nil, err
	}
	return c.dial(ctx, "/events", ticket.Ticket)
}

// dial opens a socket at an API path, authenticating with a ticket.
func (c *Client) dial(ctx context.Context, path, ticket string) (*Console, error) {
	target, err := url.Parse(c.endpoint(path, url.Values{"token": {ticket}}))
	if err != nil {
		return nil, fmt.Errorf("panelclient: building the socket address: %w", err)
	}
	// The socket lives at the same address; only the scheme differs.
	switch target.Scheme {
	case "https":
		target.Scheme = "wss"
	default:
		target.Scheme = "ws"
	}

	dialer := &websocket.Dialer{
		HandshakeTimeout: DefaultTimeout,
		// The client may carry a custom certificate pool or proxy, and a
		// self-signed panel is the default install, so the socket has to use
		// the same transport as everything else rather than its own.
		TLSClientConfig: tlsConfigOf(c.http),
		Proxy:           proxyOf(c.http),
	}

	header := http.Header{}
	header.Set("User-Agent", c.agent)
	// Origin, because the panel's default upgrade check is same-origin and a
	// request without one from a non-browser would otherwise be refused.
	header.Set("Origin", c.baseURL.String())

	conn, resp, err := dialer.DialContext(ctx, target.String(), header)
	if err != nil {
		if resp != nil {
			defer func() { _ = resp.Body.Close() }()
			// The rejection is an ordinary API error body, so it is reported
			// as one: "invalid, expired or already used ticket" is worth
			// passing on, and "bad handshake" is not.
			return nil, parseError(resp)
		}
		return nil, fmt.Errorf("panelclient: opening %s: %w", path, err)
	}
	return &Console{conn: conn}, nil
}

// Read returns the next frame. It blocks until one arrives or the connection
// ends.
func (c *Console) Read() (Frame, error) {
	var frame Frame
	if err := c.conn.ReadJSON(&frame); err != nil {
		return Frame{}, err
	}
	return frame, nil
}

// Send runs a command over the open socket.
//
// The panel answers a rejected command with an error frame rather than an
// error here: the write only fails when the connection does.
func (c *Console) Send(command string) error {
	if strings.TrimSpace(command) == "" {
		return fmt.Errorf("%w: the command is empty", ErrValidation)
	}
	payload, err := json.Marshal(map[string]string{"type": frameCommand, "text": command})
	if err != nil {
		return fmt.Errorf("panelclient: encoding the command: %w", err)
	}
	return c.conn.WriteMessage(websocket.TextMessage, payload)
}

// Close ends the connection, telling the panel why rather than dropping it.
func (c *Console) Close() error {
	_ = c.conn.WriteControl(websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
		time.Now().Add(time.Second))
	return c.conn.Close()
}
