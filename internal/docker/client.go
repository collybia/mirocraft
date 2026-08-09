// Package docker is a small client for the parts of the Docker Engine API the
// daemon needs: create a container, start it, watch its output, write to its
// stdin, stop it.
//
// Hand-written rather than the official SDK on purpose. github.com/docker/docker
// pulls in on the order of a hundred packages and several megabytes of binary
// to serve six endpoints, and the project's whole promise is one small file an
// operator copies onto a VPS. The Engine API is plain HTTP over a local socket
// and versioned conservatively; what is used here has been stable for years.
package docker

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// APIVersion is the Engine API version requested.
//
// Pinned rather than negotiated: the endpoints used here have been stable
// since well before this, and asking for a version the daemon predates is a
// clear error at startup instead of a field that silently stopped arriving.
const APIVersion = "v1.41"

// DefaultTimeout bounds a single request. Streaming endpoints override it —
// a log stream is meant to stay open.
const DefaultTimeout = 30 * time.Second

// ErrNotFound reports a container or image the daemon does not know.
var ErrNotFound = errors.New("docker: not found")

// Client talks to a Docker daemon.
type Client struct {
	http *http.Client
	// dial opens a fresh connection, for the endpoints that hijack it.
	dial func(ctx context.Context) (net.Conn, error)
	host string
}

// New connects to the Docker daemon at host.
//
// An empty host means the platform default: the unix socket, or the named pipe
// on Windows. The DOCKER_HOST form (unix://, npipe://, tcp://) is understood
// because that is what an operator already has set when their daemon is not in
// the usual place.
func New(host string) (*Client, error) {
	if host == "" {
		host = DefaultHost()
	}

	dial, err := dialerFor(host)
	if err != nil {
		return nil, err
	}

	return &Client{
		dial: dial,
		host: host,
		http: &http.Client{
			Timeout: DefaultTimeout,
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return dial(ctx)
				},
				// One connection per request keeps the hijacked attach streams
				// from sharing a pool with ordinary calls.
				DisableKeepAlives: true,
			},
		},
	}, nil
}

// Host reports where this client is pointed, for logs.
func (c *Client) Host() string { return c.host }

// --- request plumbing ---

// urlFor builds a request URL. The host part is a placeholder: the transport
// ignores it and dials the socket.
func urlFor(path string, query url.Values) string {
	u := "http://docker/" + APIVersion + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	return u
}

func (c *Client) do(ctx context.Context, method, path string, query url.Values, body any) (*http.Response, error) {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("docker: encoding the request: %w", err)
		}
		reader = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(ctx, method, urlFor(path, query), reader)
	if err != nil {
		return nil, fmt.Errorf("docker: building the request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("docker: %s %s: %w", method, path, err)
	}
	if resp.StatusCode >= 400 {
		defer func() { _ = resp.Body.Close() }()
		return nil, statusError(resp, method, path)
	}
	return resp, nil
}

// statusError turns an error response into something an operator can act on.
// The daemon puts a human-readable reason in the body, and dropping it in
// favour of the status code alone is how "500 Internal Server Error" becomes
// the whole of a bug report.
func statusError(resp *http.Response, method, path string) error {
	var payload struct {
		Message string `json:"message"`
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	_ = json.Unmarshal(raw, &payload)

	message := strings.TrimSpace(payload.Message)
	if message == "" {
		message = strings.TrimSpace(string(raw))
	}

	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("%w: %s %s: %s", ErrNotFound, method, path, message)
	}
	return fmt.Errorf("docker: %s %s: %s: %s", method, path, resp.Status, message)
}

func (c *Client) doJSON(ctx context.Context, method, path string, query url.Values, body, out any) error {
	resp, err := c.do(ctx, method, path, query, body)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("docker: decoding the response of %s %s: %w", method, path, err)
	}
	return nil
}

// --- endpoints ---

// Ping reports whether the daemon is reachable. This is what runner
// auto-selection asks before choosing Docker.
func (c *Client) Ping(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker/_ping", nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("docker: not reachable at %s: %w", c.host, err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.StatusCode >= 400 {
		return fmt.Errorf("docker: not reachable at %s: %s", c.host, resp.Status)
	}
	return nil
}

// Version reports the daemon's version, for logs.
func (c *Client) Version(ctx context.Context) (string, error) {
	var out struct {
		Version string `json:"Version"`
	}
	if err := c.doJSON(ctx, http.MethodGet, "/version", nil, nil, &out); err != nil {
		return "", err
	}
	return out.Version, nil
}

// ImageExists reports whether an image is already present locally.
func (c *Client) ImageExists(ctx context.Context, ref string) (bool, error) {
	err := c.doJSON(ctx, http.MethodGet, "/images/"+url.PathEscape(ref)+"/json", nil, nil, nil)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// PullImage fetches an image, blocking until the pull finishes.
//
// The daemon streams progress as newline-delimited JSON and reports failures
// *inside* that stream rather than as an HTTP status, so the body has to be
// read to the end to know whether the pull worked. Returning as soon as the
// request is accepted would report success for a tag that does not exist.
func (c *Client) PullImage(ctx context.Context, ref string, onProgress func(string)) error {
	// A pull is minutes, not seconds; the shared client timeout would cut it.
	pullCtx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()

	query := url.Values{"fromImage": {ref}}
	resp, err := c.do(pullCtx, http.MethodPost, "/images/create", query, nil)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	decoder := json.NewDecoder(resp.Body)
	for {
		var message struct {
			Status string `json:"status"`
			Error  string `json:"error"`
		}
		if err := decoder.Decode(&message); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("docker: reading the pull progress of %s: %w", ref, err)
		}
		if message.Error != "" {
			return fmt.Errorf("docker: pulling %s: %s", ref, message.Error)
		}
		if onProgress != nil && message.Status != "" {
			onProgress(message.Status)
		}
	}
}

// ContainerSpec is what the daemon needs to create a container.
type ContainerSpec struct {
	Name    string
	Image   string
	Cmd     []string
	Env     []string
	WorkDir string

	// Binds are host:container[:mode] mounts.
	Binds []string
	// Ports maps a container port to a host port, both TCP.
	Ports map[int]int
	// UDPPorts is the same for UDP. A separate map rather than a protocol on
	// each entry: a Java server publishes only TCP and a Bedrock server only
	// UDP, and a container that published both would hold a port nothing
	// listens on away from the server that needs it.
	UDPPorts map[int]int
	// MemoryBytes is the hard memory limit. Zero means unlimited.
	MemoryBytes int64
	// NanoCPUs limits CPU time; 1e9 is one core. Zero means unlimited.
	NanoCPUs int64

	// User is the uid:gid the container runs as. Empty means the image's own
	// user, which for most images is root.
	User string

	Labels map[string]string
}

// CreateContainer creates a container and returns its id.
func (c *Client) CreateContainer(ctx context.Context, spec ContainerSpec) (string, error) {
	exposed := map[string]struct{}{}
	bindings := map[string][]map[string]string{}
	for containerPort, hostPort := range spec.Ports {
		key := strconv.Itoa(containerPort) + "/tcp"
		exposed[key] = struct{}{}
		bindings[key] = []map[string]string{{"HostPort": strconv.Itoa(hostPort)}}
	}
	for containerPort, hostPort := range spec.UDPPorts {
		key := strconv.Itoa(containerPort) + "/udp"
		exposed[key] = struct{}{}
		bindings[key] = []map[string]string{{"HostPort": strconv.Itoa(hostPort)}}
	}

	body := map[string]any{
		"Image":        spec.Image,
		"Cmd":          spec.Cmd,
		"Env":          spec.Env,
		"User":         spec.User,
		"WorkingDir":   spec.WorkDir,
		"Labels":       spec.Labels,
		"ExposedPorts": exposed,

		// The console needs a two-way pipe that survives reattaching, which is
		// what OpenStdin without StdinOnce gives. Tty stays off: a pty would
		// merge stdout and stderr and stop the runner telling them apart.
		"OpenStdin":    true,
		"StdinOnce":    false,
		"AttachStdin":  true,
		"AttachStdout": true,
		"AttachStderr": true,
		"Tty":          false,

		"HostConfig": map[string]any{
			"Binds":        spec.Binds,
			"PortBindings": bindings,
			"Memory":       spec.MemoryBytes,
			"NanoCpus":     spec.NanoCPUs,
			// No automatic restart: the panel decides when a server runs, and a
			// container the daemon did not start is one it cannot account for.
			"RestartPolicy": map[string]any{"Name": "no"},
		},
	}

	query := url.Values{}
	if spec.Name != "" {
		query.Set("name", spec.Name)
	}

	var out struct {
		ID string `json:"Id"`
	}
	if err := c.doJSON(ctx, http.MethodPost, "/containers/create", query, body, &out); err != nil {
		return "", err
	}
	return out.ID, nil
}

// StartContainer starts a created container.
func (c *Client) StartContainer(ctx context.Context, id string) error {
	return c.doJSON(ctx, http.MethodPost, "/containers/"+id+"/start", nil, nil, nil)
}

// StopContainer asks the container to stop, killing it after timeout.
func (c *Client) StopContainer(ctx context.Context, id string, timeout time.Duration) error {
	seconds := int(timeout.Seconds())
	if seconds < 0 {
		seconds = 0
	}
	query := url.Values{"t": {strconv.Itoa(seconds)}}

	// The request itself has to outlive the stop it is asking for.
	stopCtx, cancel := context.WithTimeout(ctx, timeout+DefaultTimeout)
	defer cancel()

	return c.doJSON(stopCtx, http.MethodPost, "/containers/"+id+"/stop", query, nil, nil)
}

// KillContainer terminates the container immediately.
func (c *Client) KillContainer(ctx context.Context, id string) error {
	return c.doJSON(ctx, http.MethodPost, "/containers/"+id+"/kill", nil, nil, nil)
}

// RemoveContainer deletes a container.
func (c *Client) RemoveContainer(ctx context.Context, id string, force bool) error {
	query := url.Values{"v": {"0"}}
	if force {
		query.Set("force", "1")
	}
	return c.doJSON(ctx, http.MethodDelete, "/containers/"+id, query, nil, nil)
}

// ContainerInfo is the part of an inspect response the runner uses.
type ContainerInfo struct {
	ID    string `json:"Id"`
	Name  string `json:"Name"`
	State struct {
		Status     string `json:"Status"` // created, running, paused, restarting, removing, exited, dead
		Running    bool   `json:"Running"`
		ExitCode   int    `json:"ExitCode"`
		StartedAt  string `json:"StartedAt"`
		FinishedAt string `json:"FinishedAt"`
		OOMKilled  bool   `json:"OOMKilled"`
	} `json:"State"`
	Config struct {
		Image  string            `json:"Image"`
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
}

// InspectContainer returns a container's current state.
func (c *Client) InspectContainer(ctx context.Context, id string) (*ContainerInfo, error) {
	var info ContainerInfo
	if err := c.doJSON(ctx, http.MethodGet, "/containers/"+id+"/json", nil, nil, &info); err != nil {
		return nil, err
	}
	return &info, nil
}

// ListContainers returns containers carrying the given label, running or not.
//
// Used to find servers left behind by a previous daemon: unlike a child
// process, a container survives the daemon that made it, so it has to be found
// again rather than assumed gone.
func (c *Client) ListContainers(ctx context.Context, label string) ([]ContainerInfo, error) {
	filters, err := json.Marshal(map[string][]string{"label": {label}})
	if err != nil {
		return nil, err
	}
	query := url.Values{"all": {"1"}, "filters": {string(filters)}}

	// The list endpoint returns a flatter shape than inspect, so each is
	// inspected: a handful of containers makes the extra calls irrelevant, and
	// one shape beats two.
	var summaries []struct {
		ID string `json:"Id"`
	}
	if err := c.doJSON(ctx, http.MethodGet, "/containers/json", query, nil, &summaries); err != nil {
		return nil, err
	}

	out := make([]ContainerInfo, 0, len(summaries))
	for _, summary := range summaries {
		info, err := c.InspectContainer(ctx, summary.ID)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				continue // removed between the list and the inspect
			}
			return nil, err
		}
		out = append(out, *info)
	}
	return out, nil
}

// WaitContainer blocks until the container exits and returns its exit code.
func (c *Client) WaitContainer(ctx context.Context, id string) (int, error) {
	// No timeout: waiting is the point, and a server may run for months.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		urlFor("/containers/"+id+"/wait", nil), nil)
	if err != nil {
		return 0, err
	}

	client := &http.Client{Transport: c.http.Transport} // no timeout
	resp, err := client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("docker: waiting for %s: %w", id, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		return 0, statusError(resp, http.MethodPost, "/containers/"+id+"/wait")
	}

	var out struct {
		StatusCode int `json:"StatusCode"`
		Error      *struct {
			Message string `json:"Message"`
		} `json:"Error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return 0, fmt.Errorf("docker: decoding the wait result for %s: %w", id, err)
	}
	if out.Error != nil && out.Error.Message != "" {
		return out.StatusCode, fmt.Errorf("docker: waiting for %s: %s", id, out.Error.Message)
	}
	return out.StatusCode, nil
}

// Stats is one resource sample for a container.
type Stats struct {
	// MemoryBytes is the working set: total usage minus page cache, which is
	// what `docker stats` shows and what an operator recognises. Raw usage
	// counts every file the server has read, so a server that has streamed a
	// world through the page cache appears to be using its whole limit.
	MemoryBytes uint64
	// MemoryLimit is the container's limit, zero when unlimited.
	MemoryLimit uint64
	// CPUPercent is across all cores: 200 means two cores fully busy, the same
	// convention `docker stats` and the process runner use.
	CPUPercent float64
}

// ContainerStats takes one resource sample.
//
// stream=0 makes the daemon collect twice and return once, which is what makes
// a CPU percentage possible at all: usage counters are cumulative, so a single
// reading has nothing to be a rate of.
func (c *Client) ContainerStats(ctx context.Context, id string) (Stats, error) {
	var raw struct {
		MemoryStats struct {
			Usage uint64            `json:"usage"`
			Limit uint64            `json:"limit"`
			Stats map[string]uint64 `json:"stats"`
		} `json:"memory_stats"`
		CPUStats    cpuSample `json:"cpu_stats"`
		PreCPUStats cpuSample `json:"precpu_stats"`
	}

	// The daemon sleeps about a second between its two samples, so the shared
	// timeout would be tight.
	statsCtx, cancel := context.WithTimeout(ctx, DefaultTimeout+5*time.Second)
	defer cancel()

	query := url.Values{"stream": {"0"}}
	if err := c.doJSON(statsCtx, http.MethodGet, "/containers/"+id+"/stats", query, nil, &raw); err != nil {
		return Stats{}, err
	}

	stats := Stats{MemoryLimit: raw.MemoryStats.Limit, MemoryBytes: raw.MemoryStats.Usage}

	// cgroup v2 names it inactive_file, v1 named it cache. Neither is present
	// on every host, so a missing key means "subtract nothing" rather than an
	// error.
	for _, key := range []string{"inactive_file", "cache"} {
		if cached, ok := raw.MemoryStats.Stats[key]; ok {
			if cached < stats.MemoryBytes {
				stats.MemoryBytes -= cached
			}
			break
		}
	}

	cpuDelta := float64(raw.CPUStats.CPUUsage.TotalUsage) - float64(raw.PreCPUStats.CPUUsage.TotalUsage)
	systemDelta := float64(raw.CPUStats.SystemUsage) - float64(raw.PreCPUStats.SystemUsage)
	cores := float64(raw.CPUStats.OnlineCPUs)
	if cores == 0 {
		cores = float64(len(raw.CPUStats.CPUUsage.PerCPU))
	}
	if cpuDelta > 0 && systemDelta > 0 && cores > 0 {
		stats.CPUPercent = cpuDelta / systemDelta * cores * 100
	}
	return stats, nil
}

type cpuSample struct {
	CPUUsage struct {
		TotalUsage uint64   `json:"total_usage"`
		PerCPU     []uint64 `json:"percpu_usage"`
	} `json:"cpu_usage"`
	SystemUsage uint64 `json:"system_cpu_usage"`
	OnlineCPUs  int    `json:"online_cpus"`
}

// Attach opens a two-way connection to the container's streams.
//
// The Engine hijacks the HTTP connection here: after the response headers the
// socket carries the raw stream protocol in both directions, so this cannot go
// through the ordinary client and dials for itself.
func (c *Client) Attach(ctx context.Context, id string) (net.Conn, error) {
	conn, err := c.dial(ctx)
	if err != nil {
		return nil, fmt.Errorf("docker: dialling for attach: %w", err)
	}

	query := url.Values{
		"stream": {"1"}, "stdin": {"1"}, "stdout": {"1"}, "stderr": {"1"},
	}
	request := "POST /" + APIVersion + "/containers/" + id + "/attach?" + query.Encode() + " HTTP/1.1\r\n" +
		"Host: docker\r\n" +
		"Connection: Upgrade\r\n" +
		"Upgrade: tcp\r\n" +
		"Content-Length: 0\r\n\r\n"

	if _, err := io.WriteString(conn, request); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("docker: sending the attach request: %w", err)
	}

	// The response head has to be consumed off the socket before the stream
	// starts, and reading it any other way would swallow the first frames.
	if err := readResponseHead(conn); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return conn, nil
}

// readResponseHead consumes the status line and headers, byte by byte, so that
// nothing of the stream that follows is buffered away.
func readResponseHead(conn net.Conn) error {
	var (
		head  []byte
		one   [1]byte
		limit = 16 << 10
	)
	for len(head) < limit {
		if _, err := io.ReadFull(conn, one[:]); err != nil {
			return fmt.Errorf("docker: reading the attach response: %w", err)
		}
		head = append(head, one[0])
		if bytes.HasSuffix(head, []byte("\r\n\r\n")) {
			break
		}
	}

	status, _, _ := strings.Cut(string(head), "\r\n")
	fields := strings.Fields(status)
	if len(fields) < 2 {
		return fmt.Errorf("docker: the attach response is not HTTP: %q", status)
	}
	// 101 on an upgrade, 200 when the daemon hijacks without upgrading.
	if fields[1] != "101" && fields[1] != "200" {
		return fmt.Errorf("docker: attach refused: %s", status)
	}
	return nil
}

// --- stream framing ---

// Stream names in the multiplexed protocol.
const (
	StreamStdin  = 0
	StreamStdout = 1
	StreamStderr = 2
)

// frameHeaderSize is the size of the header before each chunk: one byte of
// stream id, three of padding, four of big-endian length.
const frameHeaderSize = 8

// DemuxFrame reads one frame from a container's output stream.
//
// Without a TTY the Engine multiplexes stdout and stderr down one connection
// with this header, so reading the socket as plain text would splice the
// length prefixes into the log.
func DemuxFrame(r io.Reader) (stream int, payload []byte, err error) {
	var header [frameHeaderSize]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return 0, nil, err
	}

	size := binary.BigEndian.Uint32(header[4:8])
	// A frame larger than this is not something a Minecraft server emits; it
	// is a desynchronised stream, and allocating on it is how a bad frame
	// becomes an out-of-memory.
	const maxFrame = 16 << 20
	if size > maxFrame {
		return 0, nil, fmt.Errorf("docker: stream frame of %d bytes is out of range", size)
	}

	payload = make([]byte, size)
	if _, err := io.ReadFull(r, payload); err != nil {
		return 0, nil, err
	}
	return int(header[0]), payload, nil
}
