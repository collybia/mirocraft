package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/collybia/mirocraft/internal/runner"
)

// Console history bounds from docs/API.md.
const (
	DefaultHistoryLines = 500
	MaxHistoryLines     = runner.ConsoleBufferLines
)

// WebSocket timings.
const (
	pingInterval = 30 * time.Second
	pongWait     = 60 * time.Second
	writeWait    = 10 * time.Second

	// outboundQueue buffers frames between the fan-out and the socket writer.
	outboundQueue = 256
)

// ConsoleService is the slice of the runner the console endpoints need.
type ConsoleService interface {
	Status(ctx context.Context, id string) (runner.Status, error)
	History(ctx context.Context, id string, lines int) ([]runner.ConsoleLine, error)
	SubscribeWithHistory(ctx context.Context, id string, lines int) ([]runner.ConsoleLine, <-chan runner.ConsoleLine, func(), error)
	SubscribeStatus(ctx context.Context, id string) (<-chan runner.Status, func(), error)
	SendCommand(ctx context.Context, id string, cmd string) error
}

// Frame types on the console socket.
const (
	frameLine    = "line"
	frameStatus  = "status"
	frameError   = "error"
	frameCommand = "command"
)

type lineFrame struct {
	Type   string    `json:"type"`
	TS     time.Time `json:"ts"`
	Stream string    `json:"stream"`
	Text   string    `json:"text"`
}

type statusFrame struct {
	Type   string `json:"type"`
	Status string `json:"status"`
}

// errorFrame reports a rejected client frame. It is an addition to the
// protocol sketched in docs/API.md: without it a client whose command was
// rejected sees silence and cannot tell whether the command ran.
type errorFrame struct {
	Type    string `json:"type"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

type clientFrame struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type historyResponse struct {
	Items []lineFrame `json:"items"`
}

type ticketResponse struct {
	Ticket    string    `json:"ticket"`
	ExpiresAt time.Time `json:"expires_at"`
}

type commandRequest struct {
	Command string `json:"command"`
}

func toLineFrame(l runner.ConsoleLine) lineFrame {
	return lineFrame{Type: frameLine, TS: l.TS, Stream: l.Stream, Text: l.Text}
}

// handleConsoleHistory serves GET /servers/{id}/console/history?lines=N.
func (a *API) handleConsoleHistory(w http.ResponseWriter, r *http.Request) {
	serverID := r.PathValue("id")
	if _, ok := a.authorizeServer(w, r, serverID, ScopeServersConsole); !ok {
		return
	}

	lines, err := parseHistoryLines(r.URL.Query().Get("lines"))
	if err != nil {
		writeErrorDetails(w, http.StatusBadRequest, CodeValidationFailed, err.Error(),
			map[string]any{"field": "lines", "max": MaxHistoryLines})
		return
	}

	history, err := a.console.History(r.Context(), serverID, lines)
	if err != nil {
		a.writeRunnerError(w, serverID, err)
		return
	}

	items := make([]lineFrame, len(history))
	for i, l := range history {
		items[i] = toLineFrame(l)
	}
	writeJSON(w, http.StatusOK, historyResponse{Items: items})
}

// parseHistoryLines applies the documented default and cap.
func parseHistoryLines(raw string) (int, error) {
	if raw == "" {
		return DefaultHistoryLines, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, errors.New("lines must be an integer")
	}
	if n <= 0 {
		return 0, errors.New("lines must be positive")
	}
	if n > MaxHistoryLines {
		n = MaxHistoryLines
	}
	return n, nil
}

// handleCommand serves POST /servers/{id}/command.
func (a *API) handleCommand(w http.ResponseWriter, r *http.Request) {
	serverID := r.PathValue("id")
	if _, ok := a.authorizeServer(w, r, serverID, ScopeServersConsole); !ok {
		return
	}

	var req commandRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, CodeValidationFailed, "body must be a json object with a command field")
		return
	}

	if err := a.console.SendCommand(r.Context(), serverID, req.Command); err != nil {
		a.writeRunnerError(w, serverID, err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// handleConsoleTicket serves POST /servers/{id}/console/ticket.
func (a *API) handleConsoleTicket(w http.ResponseWriter, r *http.Request) {
	serverID := r.PathValue("id")
	principal, ok := a.authorizeServer(w, r, serverID, ScopeServersConsole)
	if !ok {
		return
	}

	token, expiresAt, err := a.tickets.Issue(serverID, principal.UserID)
	if err != nil {
		a.log.Error("issuing console ticket failed", slog.String("error", err.Error()))
		writeError(w, http.StatusInternalServerError, CodeInternalError, "could not issue a ticket")
		return
	}

	writeJSON(w, http.StatusCreated, ticketResponse{Ticket: token, ExpiresAt: expiresAt})
}

// writeRunnerError maps runner errors onto the documented API error codes.
func (a *API) writeRunnerError(w http.ResponseWriter, serverID string, err error) {
	switch {
	case errors.Is(err, runner.ErrServerNotFound):
		writeError(w, http.StatusNotFound, CodeServerNotFound, "server "+serverID+" is not running on this node")
	case errors.Is(err, runner.ErrNotRunning):
		writeError(w, http.StatusConflict, CodeServerNotRunning, "server "+serverID+" is not running")
	case errors.Is(err, runner.ErrCommandEmpty),
		errors.Is(err, runner.ErrCommandTooLong),
		errors.Is(err, runner.ErrCommandControl),
		errors.Is(err, runner.ErrCommandNotUTF8):
		writeErrorDetails(w, http.StatusBadRequest, CodeValidationFailed, err.Error(),
			map[string]any{"field": "command", "max_length": runner.MaxCommandRunes})
	default:
		a.log.Error("console request failed",
			slog.String("server_id", serverID), slog.String("error", err.Error()))
		writeError(w, http.StatusInternalServerError, CodeInternalError, "internal error")
	}
}

// handleConsoleWS serves GET /servers/{id}/console?token={ticket}.
//
// Authorization is the ticket itself: it was issued to a principal that had
// already passed the scope and ownership checks, and it is single-use.
func (a *API) handleConsoleWS(w http.ResponseWriter, r *http.Request) {
	serverID := r.PathValue("id")

	ticket, err := a.tickets.Redeem(r.URL.Query().Get("token"))
	if err != nil {
		writeError(w, http.StatusUnauthorized, CodeUnauthorized, "invalid, expired or already used ticket")
		return
	}
	if ticket.ServerID != serverID {
		writeError(w, http.StatusForbidden, CodeForbidden, "ticket was issued for a different server")
		return
	}

	conn, err := a.upgrader.Upgrade(w, r, nil)
	if err != nil {
		// Upgrade has already written a response.
		a.log.Debug("console websocket upgrade failed", slog.String("error", err.Error()))
		return
	}

	a.runConsoleSocket(r.Context(), conn, serverID, ticket.UserID)
}

// runConsoleSocket drives one console connection until either side goes away.
func (a *API) runConsoleSocket(parent context.Context, conn *websocket.Conn, serverID, userID string) {
	ctx, cancel := context.WithCancel(context.WithoutCancel(parent))
	defer cancel()
	defer func() { _ = conn.Close() }()

	history, lines, unsubscribeLines, err := a.console.SubscribeWithHistory(ctx, serverID, DefaultHistoryLines)
	if err != nil {
		a.closeWithError(conn, CodeServerNotFound, "server is not running on this node")
		return
	}
	defer unsubscribeLines()

	statuses, unsubscribeStatus, err := a.console.SubscribeStatus(ctx, serverID)
	if err != nil {
		a.closeWithError(conn, CodeServerNotFound, "server is not running on this node")
		return
	}
	defer unsubscribeStatus()

	outbound := make(chan any, outboundQueue)
	writerDone := make(chan struct{})
	go a.writePump(conn, outbound, writerDone)

	// History first, then the live stream. SubscribeWithHistory took both under
	// one lock, so nothing can have slipped between them.
	for _, l := range history {
		select {
		case outbound <- toLineFrame(l):
		default:
			// Scrollback larger than the queue: the tail matters more than the
			// head, so older lines are dropped rather than blocking the start.
		}
	}
	if status, err := a.console.Status(ctx, serverID); err == nil {
		trySend(outbound, statusFrame{Type: frameStatus, Status: string(status)})
	}

	// Fan the two subscriptions into the writer.
	//
	// The wait group is not decoration: outbound must not be closed while this
	// goroutine can still send on it. A select with a ready ctx.Done() still
	// picks a ready line case at random, so cancelling is not enough — the
	// sender has to be observed finished before the channel closes, or a
	// disconnect that races a log line panics with "send on closed channel"
	// and takes the whole daemon, and every server it runs, down with it.
	var fanOut sync.WaitGroup
	fanOut.Add(1)
	go func() {
		defer fanOut.Done()
		defer cancel()

		for {
			select {
			case <-ctx.Done():
				return
			case l, ok := <-lines:
				if !ok {
					return // server stopped or daemon shutting down
				}
				trySend(outbound, toLineFrame(l))
			case s, ok := <-statuses:
				if !ok {
					return
				}
				trySend(outbound, statusFrame{Type: frameStatus, Status: string(s)})
			}
		}
	}()

	a.readPump(ctx, conn, serverID, userID, outbound)

	cancel()
	fanOut.Wait()
	close(outbound)
	<-writerDone
}

// trySend never blocks: a viewer that cannot keep up loses frames rather than
// stalling the fan-out for everyone else.
func trySend(outbound chan any, frame any) {
	select {
	case outbound <- frame:
	default:
	}
}

// readPump handles client frames until the socket closes.
func (a *API) readPump(ctx context.Context, conn *websocket.Conn, serverID, userID string, outbound chan any) {
	conn.SetReadLimit(16 << 10)
	_ = conn.SetReadDeadline(time.Now().Add(pongWait))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(pongWait))
	})

	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			return
		}

		var frame clientFrame
		if err := json.Unmarshal(data, &frame); err != nil {
			trySend(outbound, errorFrame{Type: frameError, Code: CodeValidationFailed, Message: "frame is not valid json"})
			continue
		}
		if frame.Type != frameCommand {
			trySend(outbound, errorFrame{Type: frameError, Code: CodeValidationFailed, Message: "unsupported frame type " + frame.Type})
			continue
		}

		if err := a.console.SendCommand(ctx, serverID, frame.Text); err != nil {
			trySend(outbound, errorFrame{
				Type:    frameError,
				Code:    consoleErrorCode(err),
				Message: err.Error(),
			})
			continue
		}

		a.log.Debug("console command accepted",
			slog.String("server_id", serverID), slog.String("user_id", userID))
	}
}

func consoleErrorCode(err error) string {
	switch {
	case errors.Is(err, runner.ErrNotRunning):
		return CodeServerNotRunning
	case errors.Is(err, runner.ErrServerNotFound):
		return CodeServerNotFound
	case errors.Is(err, runner.ErrCommandEmpty),
		errors.Is(err, runner.ErrCommandTooLong),
		errors.Is(err, runner.ErrCommandControl),
		errors.Is(err, runner.ErrCommandNotUTF8):
		return CodeValidationFailed
	default:
		return CodeInternalError
	}
}

// writePump owns the socket's write side. gorilla/websocket forbids concurrent
// writers, so every frame and every ping goes through this one goroutine.
func (a *API) writePump(conn *websocket.Conn, outbound chan any, done chan struct{}) {
	defer close(done)

	ticker := time.NewTicker(pingInterval)
	defer ticker.Stop()

	for {
		select {
		case frame, ok := <-outbound:
			if !ok {
				_ = conn.SetWriteDeadline(time.Now().Add(writeWait))
				_ = conn.WriteMessage(websocket.CloseMessage,
					websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
				return
			}
			_ = conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := conn.WriteJSON(frame); err != nil {
				return
			}
		case <-ticker.C:
			_ = conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				// The peer is gone; the read side will notice via pongWait.
				return
			}
		}
	}
}

// closeWithError sends a single error frame and closes the socket.
func (a *API) closeWithError(conn *websocket.Conn, code, message string) {
	_ = conn.SetWriteDeadline(time.Now().Add(writeWait))
	_ = conn.WriteJSON(errorFrame{Type: frameError, Code: code, Message: message})
	_ = conn.WriteMessage(websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.ClosePolicyViolation, message))
	_ = conn.Close()
}
