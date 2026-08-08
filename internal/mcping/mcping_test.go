package mcping

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"testing"
	"time"
)

// fakeServer speaks just enough of the protocol to answer one list ping.
// respond is the JSON payload it returns.
func fakeServer(t *testing.T, respond string) (host string, port int) {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	addr := listener.Addr().(*net.TCPAddr)

	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

		reader := bufio.NewReader(conn)
		// Handshake, then the empty status request; both are read and dropped.
		for i := 0; i < 2; i++ {
			length, err := readVarInt(reader)
			if err != nil {
				return
			}
			if _, err := io.CopyN(io.Discard, reader, int64(length)); err != nil {
				return
			}
		}

		var payload []byte
		payload = appendVarInt(payload, 0x00)
		payload = appendString(payload, respond)

		var framed []byte
		framed = appendVarInt(framed, int32(len(payload)))
		framed = append(framed, payload...)
		_, _ = conn.Write(framed)
	}()

	return addr.IP.String(), addr.Port
}

func TestPing(t *testing.T) {
	response := `{
		"version": {"name": "1.21.4", "protocol": 769},
		"players": {"max": 20, "online": 3},
		"description": "A Mirocraft server"
	}`
	host, port := fakeServer(t, response)

	status, err := Ping(context.Background(), host, port)
	if err != nil {
		t.Fatalf("Ping: %v", err)
	}

	if status.VersionName != "1.21.4" || status.ProtocolVersion != 769 {
		t.Errorf("version = %q/%d", status.VersionName, status.ProtocolVersion)
	}
	if status.PlayersOnline != 3 || status.PlayersMax != 20 {
		t.Errorf("players = %d/%d, want 3/20", status.PlayersOnline, status.PlayersMax)
	}
	if status.MOTD != "A Mirocraft server" {
		t.Errorf("motd = %q", status.MOTD)
	}
	if status.LatencyMs < 0 {
		t.Errorf("latency = %d", status.LatencyMs)
	}
}

// Modern servers send the MOTD as a chat component tree rather than a string.
func TestPingFlattensComponentMOTD(t *testing.T) {
	response := `{
		"version": {"name": "1.21.4", "protocol": 769},
		"players": {"max": 20, "online": 0},
		"description": {"text": "Hello, ", "extra": [{"text": "world"}, {"text": "!"}]}
	}`
	host, port := fakeServer(t, response)

	status, err := Ping(context.Background(), host, port)
	if err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if status.MOTD != "Hello, world!" {
		t.Fatalf("motd = %q, want the flattened component tree", status.MOTD)
	}
}

func TestPingMissingDescription(t *testing.T) {
	host, port := fakeServer(t, `{"version":{"name":"x","protocol":1},"players":{"max":1,"online":0}}`)

	status, err := Ping(context.Background(), host, port)
	if err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if status.MOTD != "" {
		t.Fatalf("motd = %q, want empty", status.MOTD)
	}
}

func TestPingRejectsGarbage(t *testing.T) {
	host, port := fakeServer(t, "this is not json")

	if _, err := Ping(context.Background(), host, port); err == nil {
		t.Fatal("Ping accepted a non-JSON payload")
	}
}

// Nothing listening must fail promptly rather than hang.
func TestPingUnreachable(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close() // free the port so the connection is refused

	start := time.Now()
	if _, err := Ping(context.Background(), "127.0.0.1", port); err == nil {
		t.Fatal("Ping succeeded against a closed port")
	}
	if elapsed := time.Since(start); elapsed > 2*DefaultTimeout {
		t.Fatalf("Ping took %v against a closed port", elapsed)
	}
}

func TestPingRespectsContextCancellation(t *testing.T) {
	// A listener that accepts and then says nothing.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening: %v", err)
	}
	defer func() { _ = listener.Close() }()
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		time.Sleep(10 * time.Second)
		_ = conn.Close()
	}()

	port := listener.Addr().(*net.TCPAddr).Port
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	start := time.Now()
	if _, err := Ping(ctx, "127.0.0.1", port); err == nil {
		t.Fatal("Ping succeeded against a silent server")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("Ping ignored the context deadline and took %v", elapsed)
	}
}

func TestVarIntRoundTrip(t *testing.T) {
	values := []int32{0, 1, 2, 127, 128, 255, 25565, 2147483647, -1, -2147483648}

	for _, want := range values {
		encoded := appendVarInt(nil, want)
		got, err := readVarInt(bufio.NewReader(newByteReader(encoded)))
		if err != nil {
			t.Errorf("readVarInt(%d): %v", want, err)
			continue
		}
		if got != want {
			t.Errorf("round trip of %d gave %d", want, got)
		}
	}
}

// A stream of continuation bytes must be refused rather than read forever.
func TestReadVarIntRejectsOverlongEncoding(t *testing.T) {
	overlong := []byte{0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF}

	if _, err := readVarInt(bufio.NewReader(newByteReader(overlong))); err == nil {
		t.Fatal("readVarInt accepted an overlong encoding")
	}
}

func TestAppendString(t *testing.T) {
	encoded := appendString(nil, "hello")

	reader := bufio.NewReader(newByteReader(encoded))
	length, err := readVarInt(reader)
	if err != nil {
		t.Fatalf("readVarInt: %v", err)
	}
	if length != 5 {
		t.Fatalf("length = %d, want 5", length)
	}

	body := make([]byte, length)
	if _, err := io.ReadFull(reader, body); err != nil {
		t.Fatalf("reading body: %v", err)
	}
	if string(body) != "hello" {
		t.Fatalf("body = %q", body)
	}
}

func TestDecodeDescription(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{"plain string", `"just text"`, "just text"},
		{"component", `{"text":"a","extra":[{"text":"b"}]}`, "ab"},
		{"empty", ``, ""},
		{"unexpected shape", `12345`, ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := decodeDescription(json.RawMessage(tc.raw))
			if got != tc.want {
				t.Fatalf("decodeDescription(%s) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

// The port is written as an unsigned 16-bit value, so ports above 32767 must
// survive the handshake unmangled.
func TestHandshakeEncodesHighPorts(t *testing.T) {
	for _, port := range []int{25565, 40000, 65535} {
		var buf writeCollector
		if err := writeHandshake(&buf, "example.com", port); err != nil {
			t.Fatalf("writeHandshake(%d): %v", port, err)
		}
		if len(buf.data) == 0 {
			t.Fatalf("writeHandshake(%d) wrote nothing", port)
		}
		if !containsPort(buf.data, port) {
			t.Errorf("the handshake for port %d does not contain its big-endian encoding", port)
		}
	}
}

type writeCollector struct{ data []byte }

func (w *writeCollector) Write(p []byte) (int, error) {
	w.data = append(w.data, p...)
	return len(p), nil
}

func containsPort(data []byte, port int) bool {
	hi := byte(port >> 8)
	lo := byte(port & 0xFF)
	for i := 0; i+1 < len(data); i++ {
		if data[i] == hi && data[i+1] == lo {
			return true
		}
	}
	return false
}
