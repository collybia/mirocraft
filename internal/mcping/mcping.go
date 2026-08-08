// Package mcping speaks the Minecraft Server List Ping protocol — the same
// exchange the vanilla client makes to fill in a row of the multiplayer
// server list.
//
// It is used instead of the UDP query protocol because query has to be turned
// on in server.properties (enable-query) and its port configured, while list
// ping works on the game port of every modern server with no configuration at
// all. The trade-off is that ping reports players and version but not TPS.
package mcping

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"time"
)

// DefaultTimeout bounds a whole ping exchange.
const DefaultTimeout = 3 * time.Second

// maxPacketBytes caps a response. A well-behaved server sends a few kilobytes;
// the limit stops a hostile or broken one from making the daemon allocate
// without bound.
const maxPacketBytes = 1 << 20

// Status is the result of a successful ping.
type Status struct {
	VersionName     string `json:"version_name"`
	ProtocolVersion int    `json:"protocol_version"`
	PlayersOnline   int    `json:"players_online"`
	PlayersMax      int    `json:"players_max"`
	MOTD            string `json:"motd"`
	LatencyMs       int64  `json:"latency_ms"`
}

// rawStatus mirrors the JSON a server returns.
type rawStatus struct {
	Version struct {
		Name     string `json:"name"`
		Protocol int    `json:"protocol"`
	} `json:"version"`
	Players struct {
		Max    int `json:"max"`
		Online int `json:"online"`
	} `json:"players"`
	Description json.RawMessage `json:"description"`
}

// Ping performs a list ping against host:port.
func Ping(ctx context.Context, host string, port int) (*Status, error) {
	address := net.JoinHostPort(host, strconv.Itoa(port))

	dialer := net.Dialer{Timeout: DefaultTimeout}
	start := time.Now()
	conn, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return nil, fmt.Errorf("connecting to %s: %w", address, err)
	}
	defer func() { _ = conn.Close() }()

	deadline := time.Now().Add(DefaultTimeout)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return nil, fmt.Errorf("setting deadline: %w", err)
	}

	if err := writeHandshake(conn, host, port); err != nil {
		return nil, err
	}
	if err := writePacket(conn, 0x00, nil); err != nil {
		return nil, fmt.Errorf("sending status request: %w", err)
	}

	reader := bufio.NewReader(conn)
	payload, err := readStatusPacket(reader)
	if err != nil {
		return nil, err
	}
	latency := time.Since(start).Milliseconds()

	var raw rawStatus
	if err := json.Unmarshal(payload, &raw); err != nil {
		return nil, fmt.Errorf("decoding status: %w", err)
	}

	return &Status{
		VersionName:     raw.Version.Name,
		ProtocolVersion: raw.Version.Protocol,
		PlayersOnline:   raw.Players.Online,
		PlayersMax:      raw.Players.Max,
		MOTD:            decodeDescription(raw.Description),
		LatencyMs:       latency,
	}, nil
}

// writeHandshake sends the handshake packet with next-state 1 (status).
func writeHandshake(w io.Writer, host string, port int) error {
	var body []byte
	body = appendVarInt(body, -1) // protocol version: -1 means "undefined"
	body = appendString(body, host)
	body = binary.BigEndian.AppendUint16(body, uint16(port))
	body = appendVarInt(body, 1) // next state: status

	if err := writePacket(w, 0x00, body); err != nil {
		return fmt.Errorf("sending handshake: %w", err)
	}
	return nil
}

// writePacket frames id and body as length-prefixed data.
func writePacket(w io.Writer, id int32, body []byte) error {
	var packet []byte
	packet = appendVarInt(packet, id)
	packet = append(packet, body...)

	var framed []byte
	framed = appendVarInt(framed, int32(len(packet)))
	framed = append(framed, packet...)

	_, err := w.Write(framed)
	return err
}

// readStatusPacket reads the response and returns the JSON payload.
func readStatusPacket(r *bufio.Reader) ([]byte, error) {
	length, err := readVarInt(r)
	if err != nil {
		return nil, fmt.Errorf("reading packet length: %w", err)
	}
	if length <= 0 || length > maxPacketBytes {
		return nil, fmt.Errorf("packet length %d is out of range", length)
	}

	packet := make([]byte, length)
	if _, err := io.ReadFull(r, packet); err != nil {
		return nil, fmt.Errorf("reading packet: %w", err)
	}

	inner := bufio.NewReader(newByteReader(packet))
	id, err := readVarInt(inner)
	if err != nil {
		return nil, fmt.Errorf("reading packet id: %w", err)
	}
	if id != 0x00 {
		return nil, fmt.Errorf("unexpected packet id %d", id)
	}

	payloadLen, err := readVarInt(inner)
	if err != nil {
		return nil, fmt.Errorf("reading payload length: %w", err)
	}
	if payloadLen < 0 || payloadLen > maxPacketBytes {
		return nil, fmt.Errorf("payload length %d is out of range", payloadLen)
	}

	payload := make([]byte, payloadLen)
	if _, err := io.ReadFull(inner, payload); err != nil {
		return nil, fmt.Errorf("reading payload: %w", err)
	}
	return payload, nil
}

// decodeDescription flattens the MOTD, which servers send either as a plain
// string or as a nested chat component tree.
func decodeDescription(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}

	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text
	}

	var component chatComponent
	if err := json.Unmarshal(raw, &component); err != nil {
		return ""
	}
	return component.flatten()
}

type chatComponent struct {
	Text  string          `json:"text"`
	Extra []chatComponent `json:"extra"`
}

func (c chatComponent) flatten() string {
	out := c.Text
	for _, child := range c.Extra {
		out += child.flatten()
	}
	return out
}

// --- varint helpers ---

// ErrVarIntTooLong guards against a malformed stream that would otherwise loop.
var ErrVarIntTooLong = errors.New("varint is longer than 5 bytes")

func appendVarInt(dst []byte, value int32) []byte {
	u := uint32(value)
	for {
		if u&^0x7F == 0 {
			return append(dst, byte(u))
		}
		dst = append(dst, byte(u&0x7F|0x80))
		u >>= 7
	}
}

func appendString(dst []byte, s string) []byte {
	dst = appendVarInt(dst, int32(len(s)))
	return append(dst, s...)
}

func readVarInt(r io.ByteReader) (int32, error) {
	var (
		value int32
		shift uint
	)
	for i := 0; i < 5; i++ {
		b, err := r.ReadByte()
		if err != nil {
			return 0, err
		}
		value |= int32(b&0x7F) << shift
		if b&0x80 == 0 {
			return value, nil
		}
		shift += 7
	}
	return 0, ErrVarIntTooLong
}

// byteReader adapts a slice to io.Reader without pulling in bytes.Reader's
// extra surface.
type byteReader struct {
	data []byte
	pos  int
}

func newByteReader(data []byte) *byteReader { return &byteReader{data: data} }

func (r *byteReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}
	n := copy(p, r.data[r.pos:])
	r.pos += n
	return n, nil
}
