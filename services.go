package main

import (
	"bufio"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-sql-driver/mysql"
)

// Services the agent has been told about: nginx's status page and a MySQL login. Both are off
// unless configured. From MySQL it reads counters only, with the fixed queries below: never query
// text, table names or any data.

const (
	serviceTimeout = 5 * time.Second
	statusBytes    = 4096
	errorChars     = 200
)

const (
	mysqlStatus    = "SHOW GLOBAL STATUS WHERE Variable_name IN ('Threads_connected','Threads_running','Questions','Slow_queries','Aborted_connects','Innodb_buffer_pool_reads','Innodb_buffer_pool_read_requests','Uptime')"
	mysqlLimits    = "SELECT @@max_connections, VERSION()"
	mysqlReplica   = "SHOW REPLICA STATUS"
	mysqlOldServer = "SHOW SLAVE STATUS"
)

type Service struct {
	TS      float64            `json:"ts"`
	Host    string             `json:"host"`
	Kind    string             `json:"kind"`
	Up      bool               `json:"up"`
	Error   string             `json:"error,omitempty"`
	Version string             `json:"version,omitempty"`
	Metrics map[string]float64 `json:"metrics"`
}

type Services struct {
	host   string
	nginx  string
	client *http.Client
	db     *sql.DB
	mu     sync.Mutex
	last   map[string]reading
}

type reading struct {
	at     time.Time
	values map[string]uint64
}

func NewServices(cfg Config) (*Services, error) {
	s := &Services{host: cfg.Host, nginx: cfg.NginxStatus, last: map[string]reading{}, client: &http.Client{
		Timeout:       serviceTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
	if cfg.MySQLDSN != "" {
		c, err := mysql.ParseDSN(cfg.MySQLDSN)
		if err != nil {
			return nil, errors.New("mysql_dsn isn't valid: it looks like user:password@tcp(127.0.0.1:3306)/") // not the driver's error: it can quote the password
		}
		c.Timeout, c.ReadTimeout = serviceTimeout, serviceTimeout
		db, err := sql.Open("mysql", c.FormatDSN())
		if err != nil {
			return nil, errors.New("mysql_dsn couldn't be used")
		}
		db.SetMaxOpenConns(1)
		db.SetMaxIdleConns(0) // one short visit a minute: nothing held open between
		s.db = db
	}
	return s, nil
}

func (s *Services) Enabled() bool { return s.nginx != "" || s.db != nil }

// Read checks each configured service. One that's down is a result, not a failure.
func (s *Services) Read(ctx context.Context, now time.Time) []Service {
	var out []Service
	if s.nginx != "" {
		out = append(out, s.readNginx(ctx, now))
	}
	if s.db != nil {
		out = append(out, s.readMySQL(ctx, now))
	}
	return out
}

func cleanError(err error) string {
	return cut(mask(err.Error()), errorChars)
}

// plainReason keeps the end of Go's error: "connection refused", not the whole dial it came from.
func plainReason(err error) string {
	msg := cleanError(err)
	if i := strings.LastIndex(msg, ": "); i >= 0 {
		return msg[i+2:]
	}
	return msg
}

// rate turns a running total into per-second, and into a count since the last report. A total
// that went down means the service restarted, so that interval counts as nothing.
func (s *Services) rates(kind string, now time.Time, totals map[string]uint64) (func(string) float64, func(string) float64) {
	s.mu.Lock()
	last := s.last[kind]
	s.last[kind] = reading{now, totals}
	s.mu.Unlock()
	diff := func(k string) float64 {
		if last.values == nil {
			return 0
		}
		return float64(sub(totals[k], last.values[k]))
	}
	perSecond := func(k string) float64 {
		secs := now.Sub(last.at).Seconds()
		if last.values == nil || secs <= 0 {
			return 0
		}
		return round2(diff(k) / secs)
	}
	return perSecond, diff
}

// ---- nginx -------------------------------------------------------------------------------

var nginxLine = regexp.MustCompile(`Reading:\s*(\d+)\s+Writing:\s*(\d+)\s+Waiting:\s*(\d+)`)

type nginxStatus struct{ active, accepts, handled, requests, reading, writing, waiting uint64 }

func parseNginx(b []byte) (nginxStatus, error) {
	var n nginxStatus
	lines := bufio.NewScanner(strings.NewReader(string(b)))
	found := 0
	for lines.Scan() {
		l := strings.TrimSpace(lines.Text())
		f := strings.Fields(l)
		switch {
		case strings.HasPrefix(l, "Active connections:") && len(f) == 3:
			n.active, _ = strconv.ParseUint(f[2], 10, 64)
			found++
		case len(f) == 3 && isDigits(f[0]) && isDigits(f[1]) && isDigits(f[2]):
			n.accepts, _ = strconv.ParseUint(f[0], 10, 64)
			n.handled, _ = strconv.ParseUint(f[1], 10, 64)
			n.requests, _ = strconv.ParseUint(f[2], 10, 64)
			found++
		default:
			if m := nginxLine.FindStringSubmatch(l); m != nil {
				n.reading, _ = strconv.ParseUint(m[1], 10, 64)
				n.writing, _ = strconv.ParseUint(m[2], 10, 64)
				n.waiting, _ = strconv.ParseUint(m[3], 10, 64)
				found++
			}
		}
	}
	if found != 3 {
		return n, errors.New("that page isn't nginx's stub_status")
	}
	return n, nil
}

func isDigits(s string) bool {
	_, err := strconv.ParseUint(s, 10, 64)
	return err == nil
}

func (s *Services) readNginx(ctx context.Context, now time.Time) Service {
	out := Service{TS: unix(now), Host: s.host, Kind: "nginx", Metrics: map[string]float64{}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.nginx, nil)
	if err != nil {
		out.Error = "nginx_status isn't a valid address"
		return out
	}
	req.Header.Set("user-agent", "loggerai-agent/"+version)
	res, err := s.client.Do(req)
	if err != nil {
		out.Error = plainReason(err)
		return out
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		out.Error = fmt.Sprintf("the status page answered %d", res.StatusCode)
		return out
	}
	b, _ := io.ReadAll(io.LimitReader(res.Body, statusBytes))
	n, err := parseNginx(b)
	if err != nil {
		out.Error = err.Error()
		return out
	}
	perSecond, diff := s.rates("nginx", now, map[string]uint64{"requests": n.requests, "accepts": n.accepts, "handled": n.handled})
	out.Up = true
	out.Metrics = map[string]float64{
		"active": float64(n.active), "reading": float64(n.reading), "writing": float64(n.writing), "waiting": float64(n.waiting),
		"requests_per_s": perSecond("requests"), "dropped": sub64(diff("accepts"), diff("handled")),
	}
	return out
}

func sub64(a, b float64) float64 {
	if a < b {
		return 0
	}
	return a - b
}

// ---- MySQL -------------------------------------------------------------------------------

func mysqlMetrics(status map[string]uint64, maxConn uint64, perSecond, diff func(string) float64) map[string]float64 {
	m := map[string]float64{
		"connections": float64(status["Threads_connected"]), "running": float64(status["Threads_running"]), "max_connections": float64(maxConn),
		"queries_per_s": perSecond("Questions"), "slow_queries": diff("Slow_queries"), "aborted_connects": diff("Aborted_connects"), "uptime_s": float64(status["Uptime"]),
	}
	if maxConn > 0 {
		m["connections_pct"] = round2(float64(status["Threads_connected"]) / float64(maxConn) * 100)
	}
	if asked := diff("Innodb_buffer_pool_read_requests"); asked > 0 {
		m["buffer_hit_pct"] = round2((1 - diff("Innodb_buffer_pool_reads")/asked) * 100)
	}
	return m
}

func (s *Services) readMySQL(ctx context.Context, now time.Time) Service {
	out := Service{TS: unix(now), Host: s.host, Kind: "mysql", Metrics: map[string]float64{}}
	read, cancel := context.WithTimeout(ctx, 2*serviceTimeout)
	defer cancel()
	rows, err := s.db.QueryContext(read, mysqlStatus)
	if err != nil {
		out.Error = cleanError(err)
		return out
	}
	status := map[string]uint64{}
	for rows.Next() {
		var k, v string
		if rows.Scan(&k, &v) == nil {
			status[k], _ = strconv.ParseUint(v, 10, 64)
		}
	}
	rows.Close()
	var maxConn uint64
	if err := s.db.QueryRowContext(read, mysqlLimits).Scan(&maxConn, &out.Version); err != nil {
		out.Error = cleanError(err)
		return out
	}
	out.Version = cut(out.Version, 60)
	perSecond, diff := s.rates("mysql", now, status)
	out.Up = true
	out.Metrics = mysqlMetrics(status, maxConn, perSecond, diff)
	if lag, ok := s.replicaLag(read); ok {
		out.Metrics["replica_lag_s"] = lag
	}
	return out
}

// The login may not be allowed this, and most servers aren't replicas. Neither is a problem.
func (s *Services) replicaLag(ctx context.Context) (float64, bool) {
	for _, q := range []string{mysqlReplica, mysqlOldServer} {
		rows, err := s.db.QueryContext(ctx, q)
		if err != nil {
			continue
		}
		defer rows.Close()
		cols, err := rows.Columns()
		if err != nil || !rows.Next() {
			return 0, false
		}
		vals := make([]sql.NullString, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if rows.Scan(ptrs...) != nil {
			return 0, false
		}
		for i, c := range cols {
			if (c == "Seconds_Behind_Source" || c == "Seconds_Behind_Master") && vals[i].Valid {
				lag, err := strconv.ParseFloat(vals[i].String, 64)
				return lag, err == nil
			}
		}
		return 0, false
	}
	return 0, false
}
