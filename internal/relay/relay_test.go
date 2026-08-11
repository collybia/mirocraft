package relay

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The whole point, end to end: something on a machine nobody can reach becomes
// reachable at a public address, and the bytes arrive unchanged in both
// directions. Run against real sockets rather than fakes — the parts worth
// doubting are the pairing and the copying, and a fake connection has neither.
func TestAPlayerReachesTheServerBehindTheRelay(t *testing.T) {
	local := echoServer(t)
	relayAddr, publicPort := startRelay(t, "секрет")
	startAgent(t, relayAddr, "секрет", local)

	player, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(publicPort), 5*time.Second)
	if err != nil {
		t.Fatalf("dialling the public port: %v", err)
	}
	defer func() { _ = player.Close() }()

	if _, err := io.WriteString(player, "привет\n"); err != nil {
		t.Fatalf("writing: %v", err)
	}
	_ = player.SetReadDeadline(time.Now().Add(5 * time.Second))
	got, err := bufio.NewReader(player).ReadString('\n')
	if err != nil {
		t.Fatalf("reading: %v", err)
	}
	if got != "эхо: привет\n" {
		t.Fatalf("got %q", got)
	}
}

// Two players at once is the normal case for a game server, and the reason a
// session is dialled per player rather than multiplexed: if the pairing were
// wrong, one player would be handed the other's connection.
func TestTwoPlayersDoNotGetEachOthersConnections(t *testing.T) {
	local := echoServer(t)
	relayAddr, publicPort := startRelay(t, "секрет")
	startAgent(t, relayAddr, "секрет", local)

	type result struct {
		sent string
		got  string
	}
	results := make(chan result, 2)

	for _, word := range []string{"первый", "второй"} {
		go func(word string) {
			conn, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(publicPort), 5*time.Second)
			if err != nil {
				results <- result{word, "dial: " + err.Error()}
				return
			}
			defer func() { _ = conn.Close() }()

			_, _ = io.WriteString(conn, word+"\n")
			_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
			line, err := bufio.NewReader(conn).ReadString('\n')
			if err != nil {
				results <- result{word, "read: " + err.Error()}
				return
			}
			results <- result{word, strings.TrimSpace(line)}
		}(word)
	}

	for i := 0; i < 2; i++ {
		got := <-results
		if want := "эхо: " + got.sent; got.got != want {
			t.Errorf("player %q received %q, want %q", got.sent, got.got, want)
		}
	}
}

// A relay is on a public address, so the token is the only thing between a
// stranger and somebody's home machine.
func TestTheRelayRefusesAnUnknownToken(t *testing.T) {
	relayAddr, _ := startRelay(t, "секрет")

	conn, err := net.DialTimeout("tcp", relayAddr, 5*time.Second)
	if err != nil {
		t.Fatalf("dialling: %v", err)
	}
	defer func() { _ = conn.Close() }()

	if err := WriteMessage(conn, verbHello, "не тот токен"); err != nil {
		t.Fatalf("writing hello: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	msg, err := ReadMessage(bufio.NewReaderSize(conn, MaxLineBytes))
	if err != nil {
		t.Fatalf("reading: %v", err)
	}
	if msg.Verb != verbError {
		t.Fatalf("verb = %q, want %q", msg.Verb, verbError)
	}
}

// A session id is pairing material: whoever names one is handed the player
// waiting on it. Guessing must not be a way in, and neither must claiming one
// with a token that is not this tunnel's.
func TestASessionCannotBeClaimedWithoutTheToken(t *testing.T) {
	local := echoServer(t)
	relayAddr, publicPort := startRelay(t, "секрет")
	startAgent(t, relayAddr, "секрет", local)

	// A player arrives, which puts a session in the waiting map.
	player, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(publicPort), 5*time.Second)
	if err != nil {
		t.Fatalf("dialling: %v", err)
	}
	defer func() { _ = player.Close() }()

	// A stranger tries to claim sessions with the wrong token. Every id is
	// wrong too, but the token is checked first and on its own.
	for i := 0; i < 5; i++ {
		conn, err := net.DialTimeout("tcp", relayAddr, 5*time.Second)
		if err != nil {
			t.Fatalf("dialling: %v", err)
		}
		_ = WriteMessage(conn, verbSession, "не тот токен", strings.Repeat("ab", SessionIDBytes))
		// The relay closes on a bad token and says nothing, which is what a
		// read of zero bytes means here.
		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		if n, _ := conn.Read(make([]byte, 1)); n != 0 {
			t.Error("the relay answered a session claim with an unknown token")
		}
		_ = conn.Close()
	}

	// And the player still works, because none of that disturbed the pairing.
	_, _ = io.WriteString(player, "жив\n")
	_ = player.SetReadDeadline(time.Now().Add(5 * time.Second))
	got, err := bufio.NewReader(player).ReadString('\n')
	if err != nil {
		t.Fatalf("the player broke after the intrusion attempts: %v", err)
	}
	if strings.TrimSpace(got) != "эхо: жив" {
		t.Fatalf("got %q", got)
	}
}

// A tunnel with nobody at the home end must not hold a player forever. The
// client shows "cannot connect", which is the truth.
func TestAPlayerIsDroppedWhenNoAgentIsConnected(t *testing.T) {
	_, publicPort := startRelay(t, "секрет")

	conn, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(publicPort), 5*time.Second)
	if err != nil {
		t.Fatalf("dialling: %v", err)
	}
	defer func() { _ = conn.Close() }()

	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Read(make([]byte, 1)); err == nil {
		t.Fatal("the relay kept a player connected with no agent behind it")
	}
}

// The token is stored as a hash, like every other token in this project: a
// relay is a public machine, and a file of live credentials on it is a file
// worth stealing.
func TestTokensAreStoredAsHashes(t *testing.T) {
	token, err := NewToken()
	if err != nil {
		t.Fatalf("NewToken: %v", err)
	}
	hash := HashToken(token)

	if strings.Contains(hash, token) {
		t.Error("the hash contains the token")
	}
	if len(hash) != 64 {
		t.Errorf("hash length = %d, want 64", len(hash))
	}
	if HashToken(token) != hash {
		t.Error("hashing is not deterministic")
	}
	if HashToken(token+"x") == hash {
		t.Error("a different token hashed the same")
	}
}

func TestProtocolRoundTrip(t *testing.T) {
	var buf strings.Builder
	if err := WriteMessage(&buf, verbDial, "abc123"); err != nil {
		t.Fatalf("WriteMessage: %v", err)
	}

	msg, err := ReadMessage(bufio.NewReader(strings.NewReader(buf.String())))
	if err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	if msg.Verb != verbDial || msg.Arg(0) != "abc123" {
		t.Fatalf("round trip gave %+v", msg)
	}
	if msg.Arg(7) != "" {
		t.Error("a missing argument should read as empty, not panic")
	}

	// A peer that never sends a newline must not make the other side allocate
	// without bound.
	long := strings.Repeat("x", MaxLineBytes*2)
	if _, err := ReadMessage(bufio.NewReaderSize(strings.NewReader(long+"\n"), MaxLineBytes)); err == nil {
		t.Error("an over-long line was accepted")
	}
	if err := WriteMessage(io.Discard, verbDial, long); !errors.Is(err, ErrTooLong) {
		t.Errorf("writing an over-long message = %v, want %v", err, ErrTooLong)
	}
}

// --- helpers ---

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// echoServer stands in for a Minecraft server: something listening locally
// that the outside world cannot reach.
func echoServer(t *testing.T) string {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = conn.Close() }()
				reader := bufio.NewReader(conn)
				for {
					line, err := reader.ReadString('\n')
					if err != nil {
						return
					}
					if _, err := io.WriteString(conn, "эхо: "+line); err != nil {
						return
					}
				}
			}()
		}
	}()

	return ln.Addr().String()
}

// startRelay runs a relay on two free ports and returns its control address
// and public port.
func startRelay(t *testing.T, token string) (string, int) {
	t.Helper()
	return startRelayWith(t, token, 0)
}

func startRelayWith(t *testing.T, token string, handshake time.Duration) (string, int) {
	t.Helper()

	control := freePort(t)
	public := freePort(t)

	server := NewServer(Config{
		ControlAddr: "127.0.0.1:" + strconv.Itoa(control),
		Tunnels: []Tunnel{{
			Name: "тест", TokenHash: HashToken(token), Port: public,
		}},
		HandshakeTimeout: handshake,
		Log:              quietLogger(),
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = server.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	waitForPort(t, control)
	waitForPort(t, public)
	return "127.0.0.1:" + strconv.Itoa(control), public
}

func startAgent(t *testing.T, relayAddr, token, target string) *Agent {
	t.Helper()

	agent := NewAgent(AgentConfig{
		Addr: relayAddr, Token: token, Target: target,
		Insecure: true, Log: quietLogger(),
	})

	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = agent.Run(ctx) }()
	t.Cleanup(cancel)

	// Wait for the tunnel to be up, so the test is not racing the handshake.
	ready, stop := context.WithTimeout(context.Background(), 10*time.Second)
	defer stop()
	if port := agent.Port(ready); port == 0 {
		t.Fatal("the agent never reported a public port")
	}
	return agent
}

// freePort asks the operating system for a port and gives it straight back.
//
// A hardcoded port makes a test that fails on whichever machine happens to be
// using it, and this suite has already been bitten by a real Minecraft server
// listening on a developer's laptop.
func freePort(t *testing.T) int {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("finding a free port: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port
}

func waitForPort(t *testing.T, port int) {
	t.Helper()

	for i := 0; i < 100; i++ {
		conn, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(port), time.Second)
		if err == nil {
			_ = conn.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("nothing came up on port %d", port)
}

// The bug a live test found and this suite could not: the handshake deadline
// covers reads and writes both, and clearing only the read half left the
// control connection unable to write ten seconds later. The tunnel came up,
// looked healthy, and dropped on the first player to arrive after that — which
// is every real player, since nobody connects within milliseconds of the
// panel starting.
func TestTheTunnelStillWorksAfterTheHandshakeDeadlineWouldHavePassed(t *testing.T) {
	local := echoServer(t)
	relayAddr, publicPort := startRelayWith(t, "секрет", 150*time.Millisecond)
	startAgent(t, relayAddr, "секрет", local)

	// Past the point where the old deadline would have expired.
	time.Sleep(400 * time.Millisecond)

	player, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(publicPort), 5*time.Second)
	if err != nil {
		t.Fatalf("dialling: %v", err)
	}
	defer func() { _ = player.Close() }()

	if _, err := io.WriteString(player, "поздний\n"); err != nil {
		t.Fatalf("writing: %v", err)
	}
	_ = player.SetReadDeadline(time.Now().Add(5 * time.Second))
	got, err := bufio.NewReader(player).ReadString('\n')
	if err != nil {
		t.Fatalf("a player arriving after the handshake window got nothing: %v", err)
	}
	if strings.TrimSpace(got) != "эхо: поздний" {
		t.Fatalf("got %q", got)
	}
}
