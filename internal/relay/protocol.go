// Package relay gives a server on somebody's home machine a public address.
//
// The panel can already tell people which address to hand out and can ask a
// router to forward a port. Neither helps the case that sends people to
// Hamachi in the first place: an internet provider that hands out a shared
// address, where no router setting can work. The answer that asks the least of
// everyone is a machine with a public address that forwards for them - and the
// friends who join type an ordinary address and port, install nothing, and
// register nowhere.
//
// The relay is a separate binary because it runs somewhere else: on a VPS, not
// on the machine with the game on it. It is in this repository, and its
// protocol is here, so that nobody has to take one on trust from a service.
//
// # How a connection is made
//
// The agent - the daemon on the home machine - dials the relay and keeps one
// control connection open. It is outbound, so no port needs opening at the
// home end, which is the whole point.
//
//	agent → relay   HELLO <token>
//	relay → agent   READY <public port>
//
// When a player connects to the public port, the relay does not push the bytes
// down the control connection. It asks the agent to dial back:
//
//	relay → agent   DIAL <session>
//	agent → relay   SESSION <token> <session>     (a second connection)
//
// and then copies bytes between the player and that second connection. One
// connection per player rather than a multiplexer over the control link: a
// stream multiplexer is a few hundred lines of framing, flow control and
// head-of-line blocking to get wrong, and a Minecraft server has tens of
// players, not tens of thousands. What it costs is one extra round trip while
// the player's client is already showing a connecting screen.
//
// Every line is UTF-8, newline-terminated, and bounded; the relay speaks first
// only after the agent has proven itself.
package relay

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// Protocol verbs.
const (
	verbHello   = "HELLO"
	verbReady   = "READY"
	verbDial    = "DIAL"
	verbSession = "SESSION"
	verbError   = "ERROR"
	verbPing    = "PING"
	verbPong    = "PONG"
)

// MaxLineBytes bounds a protocol line. Nothing legitimate comes close; the
// limit is here so that a peer cannot make the other side allocate without
// bound by never sending a newline.
const MaxLineBytes = 512

// SessionIDBytes is the length of a session identifier in raw bytes before
// hex encoding. Long enough that guessing one is not a way in: a session is
// pairing material, and pairing with somebody else's player would hand them a
// connection to a server they were never offered.
const SessionIDBytes = 16

// Errors the protocol itself can produce.
var (
	ErrMalformed = errors.New("relay: malformed message")
	ErrTooLong   = errors.New("relay: message too long")
)

// Message is one parsed protocol line.
type Message struct {
	Verb string
	Args []string
}

// Arg returns the argument at i, or "" when there is none. Callers check the
// verb and then read what they expect; a missing argument is a malformed
// message, not a panic.
func (m Message) Arg(i int) string {
	if i < 0 || i >= len(m.Args) {
		return ""
	}
	return m.Args[i]
}

// WriteMessage writes one line.
func WriteMessage(w io.Writer, verb string, args ...string) error {
	line := verb
	if len(args) > 0 {
		line += " " + strings.Join(args, " ")
	}
	if len(line)+1 > MaxLineBytes {
		return ErrTooLong
	}
	if _, err := io.WriteString(w, line+"\n"); err != nil {
		return fmt.Errorf("relay: writing %s: %w", verb, err)
	}
	return nil
}

// ReadMessage reads one line from r.
//
// The reader is the caller's, and it must be the same one across calls: a
// bufio.Reader may hold bytes past the newline, and a fresh one per message
// would eat the beginning of the next.
func ReadMessage(r *bufio.Reader) (Message, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		// A partial line before EOF is not a message, and treating it as one
		// would let a peer that died mid-write be read as a valid command.
		if errors.Is(err, io.EOF) && line == "" {
			return Message{}, io.EOF
		}
		return Message{}, err
	}
	if len(line) > MaxLineBytes {
		return Message{}, ErrTooLong
	}

	fields := strings.Fields(strings.TrimRight(line, "\r\n"))
	if len(fields) == 0 {
		return Message{}, ErrMalformed
	}
	return Message{Verb: fields[0], Args: fields[1:]}, nil
}

// ParsePort reads a port from a protocol argument.
func ParsePort(value string) (int, error) {
	port, err := strconv.Atoi(value)
	if err != nil || port < 1 || port > 65535 {
		return 0, fmt.Errorf("relay: %q is not a port", value)
	}
	return port, nil
}
