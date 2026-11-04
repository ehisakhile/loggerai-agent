package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	maxContainers   = 50
	logTailMax      = 200    // per container, per read
	logTailMin      = 16     // 50 containers at this still fit a batch, so none can crowd out the rest
	logLineBytes    = 2000   // longer lines are cut
	logBatchLines   = 800    // per read, all containers together
	logBatchBytes   = 400000 // well under the API's 1 MB a batch
	logReadBytes    = 2 << 20
	dockerTimeout   = 8 * time.Second
	dockerParallel  = 5
	noLogsLabel     = "loggerai.logs"
	defaultDockerSk = "/var/run/docker.sock"
)

// Docker's socket can't be opened read-only, so the limit is here: anything but these reads is
// refused before it reaches the socket. Note what's missing: exec, start, stop, create, delete.
var dockerReads = regexp.MustCompile(`^/containers/(json|[0-9a-f]{12,64}/(json|stats|logs))$`)

type readOnly struct{ next http.RoundTripper }

func (r readOnly) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method != http.MethodGet || !dockerReads.MatchString(req.URL.Path) {
		return nil, fmt.Errorf("refused: the agent only reads from Docker (%s %s)", req.Method, req.URL.Path)
	}
	return r.next.RoundTrip(req)
}

type Container struct {
	TS        float64 `json:"ts"`
	Host      string  `json:"host"`
	Name      string  `json:"name"`
	Image     string  `json:"image"`
	State     string  `json:"state"`
	Health    string  `json:"health,omitempty"`
	Status    string  `json:"status"`
	ExitCode  int     `json:"exit_code"`
	OOMKilled bool    `json:"oom_killed"`
	StartedAt string  `json:"started_at,omitempty"`
	Restarts  int     `json:"restart_count"`
	Restarted int     `json:"restarts"` // since the last report
	CPUPct    float64 `json:"cpu_pct"`
	MemMB     float64 `json:"mem_mb"`
	MemMaxMB  float64 `json:"mem_limit_mb"`
	NetInBps  float64 `json:"net_in_bps"`
	NetOutBps float64 `json:"net_out_bps"`
}

type LogLine struct {
	TS        string `json:"ts"`
	Host      string `json:"host"`
	Container string `json:"container"`
	Stream    string `json:"stream"`
	Message   string `json:"message"`
}

// Only these fields are ever decoded. A container's environment and command, which are often full
// of secrets, are in Docker's answers too, and are dropped here without being kept.
type dockerListItem struct {
	ID     string            `json:"Id"`
	Names  []string          `json:"Names"`
	Image  string            `json:"Image"`
	State  string            `json:"State"`
	Status string            `json:"Status"`
	Labels map[string]string `json:"Labels"`
}

type dockerInspect struct {
	RestartCount int `json:"RestartCount"`
	State        struct {
		ExitCode  int    `json:"ExitCode"`
		OOMKilled bool   `json:"OOMKilled"`
		StartedAt string `json:"StartedAt"`
		Health    *struct {
			Status string `json:"Status"`
		} `json:"Health"`
	} `json:"State"`
	Config struct {
		Tty bool `json:"Tty"`
	} `json:"Config"`
}

type dockerStats struct {
	CPU struct {
		Usage struct {
			Total uint64 `json:"total_usage"`
		} `json:"cpu_usage"`
		System uint64 `json:"system_cpu_usage"`
	} `json:"cpu_stats"`
	Memory struct {
		Usage uint64            `json:"usage"`
		Limit uint64            `json:"limit"`
		Stats map[string]uint64 `json:"stats"`
	} `json:"memory_stats"`
	Networks map[string]struct {
		Rx uint64 `json:"rx_bytes"`
		Tx uint64 `json:"tx_bytes"`
	} `json:"networks"`
}

type lastContainer struct {
	at          time.Time
	cpu, system uint64
	rx, tx      uint64
	restarts    int
	known       bool // read before, so a higher restart count means restarts
	sinceReport bool // has CPU and network numbers to compare with
}

type logCursor struct {
	since time.Time // lines at or before this have been read
	tty   bool
	name  string
	skip  bool
}

type Docker struct {
	client *http.Client
	host   string
	logs   bool
	mu     sync.Mutex
	last   map[string]lastContainer
	cursor map[string]*logCursor
	warned string
}

func NewDocker(socket, host string, logs bool) *Docker {
	if socket == "" {
		socket = defaultDockerSk
	}
	dial := func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}
	return &Docker{
		client: &http.Client{Transport: readOnly{&http.Transport{DialContext: dial}}, Timeout: dockerTimeout},
		host:   host, logs: logs, last: map[string]lastContainer{}, cursor: map[string]*logCursor{},
	}
}

func (d *Docker) get(ctx context.Context, path string, q url.Values) (*http.Response, error) {
	u := url.URL{Scheme: "http", Host: "docker", Path: path, RawQuery: q.Encode()}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	res, err := d.client.Do(req)
	if err != nil {
		return nil, err
	}
	if res.StatusCode != http.StatusOK {
		res.Body.Close()
		return nil, fmt.Errorf("docker answered %d for %s", res.StatusCode, path)
	}
	return res, nil
}

func (d *Docker) getJSON(ctx context.Context, path string, q url.Values, out any) error {
	res, err := d.get(ctx, path, q)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	return json.NewDecoder(io.LimitReader(res.Body, 8<<20)).Decode(out)
}

func (d *Docker) warn(err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if msg := err.Error(); msg != d.warned { // one line per problem
		log.Printf("reading Docker failed: %s", msg)
		d.warned = msg
	}
}

func (d *Docker) list(ctx context.Context) ([]dockerListItem, error) {
	var items []dockerListItem
	err := d.getJSON(ctx, "/containers/json", url.Values{"all": {"1"}}, &items)
	sort.SliceStable(items, func(i, j int) bool { return (items[i].State == "running") && (items[j].State != "running") })
	if len(items) > maxContainers {
		items = items[:maxContainers]
	}
	return items, err
}

func containerName(c dockerListItem) string {
	if len(c.Names) > 0 {
		return strings.TrimPrefix(c.Names[0], "/")
	}
	return c.ID[:min(12, len(c.ID))]
}

// Memory as `docker stats` shows it: without the file cache, which the kernel can free.
func memoryUsed(s dockerStats) uint64 {
	for _, k := range []string{"inactive_file", "total_inactive_file"} {
		if v, ok := s.Memory.Stats[k]; ok && v < s.Memory.Usage {
			return s.Memory.Usage - v
		}
	}
	return s.Memory.Usage
}

// fill works out a container's numbers against the last report, as share of the machine, like
// processes: 25% means a quarter of all its cores.
func fill(c *Container, s dockerStats, last lastContainer, now time.Time) lastContainer {
	var rx, tx uint64
	for _, n := range s.Networks {
		rx += n.Rx
		tx += n.Tx
	}
	c.MemMB = round2(float64(memoryUsed(s)) / 1048576)
	c.MemMaxMB = round2(float64(s.Memory.Limit) / 1048576)
	next := lastContainer{at: now, cpu: s.CPU.Usage.Total, system: s.CPU.System, rx: rx, tx: tx, restarts: c.Restarts, known: true, sinceReport: true}
	if !last.sinceReport {
		return next
	}
	if d := sub(s.CPU.System, last.system); d > 0 {
		c.CPUPct = round2(float64(sub(s.CPU.Usage.Total, last.cpu)) / float64(d) * 100)
	}
	if secs := now.Sub(last.at).Seconds(); secs > 0 {
		c.NetInBps, c.NetOutBps = float64(int64(float64(sub(rx, last.rx))/secs)), float64(int64(float64(sub(tx, last.tx))/secs))
	}
	return next
}

// Containers reads every container. A problem with one never stops the others.
func (d *Docker) Containers(ctx context.Context, now time.Time) []Container {
	items, err := d.list(ctx)
	if err != nil {
		d.warn(err)
		return nil
	}
	out := make([]Container, len(items))
	seen := make(map[string]bool, len(items))
	gate := make(chan struct{}, dockerParallel)
	var wg sync.WaitGroup
	for i, it := range items {
		seen[it.ID] = true
		wg.Add(1)
		gate <- struct{}{}
		go func(i int, it dockerListItem) {
			defer wg.Done()
			defer func() { <-gate }()
			out[i] = d.read(ctx, it, now)
		}(i, it)
	}
	wg.Wait()
	d.mu.Lock()
	defer d.mu.Unlock()
	for id := range d.last {
		if !seen[id] {
			delete(d.last, id)
		}
	}
	return out
}

func (d *Docker) read(ctx context.Context, it dockerListItem, now time.Time) Container {
	c := Container{TS: unix(now), Host: d.host, Name: containerName(it), Image: it.Image, State: it.State, Status: it.Status}
	var in dockerInspect
	if err := d.getJSON(ctx, "/containers/"+it.ID+"/json", nil, &in); err == nil {
		c.Restarts, c.ExitCode, c.OOMKilled, c.StartedAt = in.RestartCount, in.State.ExitCode, in.State.OOMKilled, in.State.StartedAt
		if in.State.Health != nil {
			c.Health = in.State.Health.Status
		}
	}
	d.mu.Lock()
	last := d.last[it.ID]
	d.mu.Unlock()
	// Apart from the numbers below: a crash loop can be mid-restart at every single reading.
	if last.known && c.Restarts > last.restarts {
		c.Restarted = c.Restarts - last.restarts
	}
	next := last
	next.restarts, next.known = c.Restarts, true
	if it.State == "running" {
		var s dockerStats
		q := url.Values{"stream": {"false"}, "one-shot": {"true"}} // one reading, straight away: the change is worked out here
		if err := d.getJSON(ctx, "/containers/"+it.ID+"/stats", q, &s); err == nil {
			next = fill(&c, s, last, now)
		}
	}
	d.mu.Lock()
	d.last[it.ID] = next
	d.mu.Unlock()
	return c
}

// ---- logs --------------------------------------------------------------------------------

// splitFrames takes Docker's log stream apart. Without a terminal, each chunk starts with 8 bytes:
// the stream (1 stdout, 2 stderr), three zeros, and the length.
func splitFrames(b []byte, tty bool) []struct{ stream, text string } {
	var out []struct{ stream, text string }
	add := func(stream string, chunk []byte) {
		for _, l := range strings.Split(string(chunk), "\n") {
			if l = strings.TrimRight(l, "\r"); l != "" {
				out = append(out, struct{ stream, text string }{stream, l})
			}
		}
	}
	if tty {
		add("stdout", b)
		return out
	}
	for len(b) >= 8 {
		n := int(binary.BigEndian.Uint32(b[4:8]))
		end := min(8+n, len(b))
		stream := "stdout"
		if b[0] == 2 {
			stream = "stderr"
		}
		add(stream, b[8:end])
		b = b[end:]
	}
	return out
}

func cut(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) { // never through the middle of a character
		n--
	}
	return s[:n]
}

// logTail shares the batch evenly: however loud one container is, the others still get their say.
func logTail(containers int) int {
	return min(max(logBatchLines/max(containers, 1), logTailMin), logTailMax)
}

// parseLine splits "2026-11-04T22:00:00.123456789Z the message".
func parseLine(l string) (time.Time, string, bool) {
	ts, msg, ok := strings.Cut(l, " ")
	if !ok {
		ts, msg = l, ""
	}
	t, err := time.Parse(time.RFC3339Nano, ts)
	return t, msg, err == nil
}

func (d *Docker) cursorFor(ctx context.Context, it dockerListItem, now time.Time) *logCursor {
	c := d.cursor[it.ID]
	if c != nil {
		return c
	}
	c = &logCursor{since: now, name: containerName(it), skip: it.Labels[noLogsLabel] == "false"} // from now on: no old logs on install
	var in dockerInspect
	if err := d.getJSON(ctx, "/containers/"+it.ID+"/json", nil, &in); err != nil {
		return nil
	}
	c.tty = in.Config.Tty
	d.cursor[it.ID] = c
	return c
}

// Logs reads what each running container has written since the last read. They're masked here,
// so a secret in a log line never leaves the machine in the first place.
func (d *Docker) Logs(ctx context.Context, now time.Time) []LogLine {
	if !d.logs {
		return nil
	}
	items, err := d.list(ctx)
	if err != nil {
		d.warn(err)
		return nil
	}
	var out []LogLine
	size := 0
	seen := make(map[string]bool, len(items))
	tail := logTail(len(items))
	for _, it := range items {
		seen[it.ID] = true
		if it.State != "running" && d.cursor[it.ID] == nil {
			continue
		}
		c := d.cursorFor(ctx, it, now)
		if c == nil || c.skip {
			continue
		}
		q := url.Values{"stdout": {"1"}, "stderr": {"1"}, "timestamps": {"1"}, "tail": {strconv.Itoa(tail)},
			"since": {strconv.FormatFloat(float64(c.since.UnixNano())/1e9, 'f', 6, 64)}}
		res, err := d.get(ctx, "/containers/"+it.ID+"/logs", q)
		if err != nil {
			d.warn(err)
			continue
		}
		b, err := io.ReadAll(io.LimitReader(res.Body, logReadBytes))
		res.Body.Close()
		if err != nil && !errors.Is(err, io.EOF) {
			d.warn(err)
			continue
		}
		for _, f := range splitFrames(b, c.tty) {
			t, msg, ok := parseLine(f.text)
			if !ok || !t.After(c.since) { // "since" rounds, so a line can come back twice
				continue
			}
			c.since = t
			if len(out) >= logBatchLines || size >= logBatchBytes {
				continue // over budget: moves on rather than falling further behind
			}
			msg = mask(cut(msg, logLineBytes))
			size += len(msg)
			out = append(out, LogLine{TS: t.UTC().Format(time.RFC3339Nano), Host: d.host, Container: c.name, Stream: f.stream, Message: msg})
		}
	}
	for id := range d.cursor {
		if !seen[id] {
			delete(d.cursor, id)
		}
	}
	return out
}
