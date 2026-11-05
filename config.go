package main

import (
	"bufio"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"strings"
)

const (
	defaultConfig   = "/etc/loggerai/agent.conf"
	defaultEndpoint = "https://loggerapp.ehigiator.com/api/v1/ingest/batch"
)

type Config struct {
	Key         string
	Endpoint    string
	Environment string
	Host        string
	AllowHTTP   bool
	Docker      bool // read containers: off unless the installer was told to
	DockerLogs  bool // and send what they log
	DockerSock  string
	NginxStatus string // nginx's stub_status page
	MySQLDSN    string // a login that can read status: user:password@tcp(127.0.0.1:3306)/
}

func procRoot() string {
	if p := os.Getenv("LOGGERAI_PROC"); p != "" {
		return p
	}
	return "/proc"
}

// readConfig parses key=value lines. Blank lines and lines starting with # are skipped.
func readConfig(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if st, err := f.Stat(); err == nil && st.Mode().Perm()&0o004 != 0 {
		log.Printf("warning: %s can be read by every user on this machine, and it holds your secret key. Run: chmod 640 %s", path, path)
	}
	out := map[string]string{}
	lines := bufio.NewScanner(f)
	for lines.Scan() {
		line := strings.TrimSpace(lines.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		out[strings.ToLower(strings.TrimSpace(k))] = strings.Trim(strings.TrimSpace(v), `"'`)
	}
	return out, lines.Err()
}

func loadConfig(needKey bool) (Config, error) {
	path := os.Getenv("LOGGERAI_CONFIG")
	named := path != ""
	if !named {
		path = defaultConfig
	}
	file, err := readConfig(path)
	if err != nil {
		// Without the file, the environment may still say everything; a file someone named must exist.
		if named || !errors.Is(err, os.ErrNotExist) {
			return Config{}, fmt.Errorf("reading %s: %w", path, err)
		}
		file = map[string]string{}
	}
	pick := func(name, env, fallback string) string {
		if v := os.Getenv(env); v != "" {
			return v
		}
		if v := file[name]; v != "" {
			return v
		}
		return fallback
	}
	host, _ := os.Hostname()
	cfg := Config{
		Key:         pick("key", "LOGGERAI_KEY", ""),
		Endpoint:    pick("endpoint", "LOGGERAI_ENDPOINT", defaultEndpoint),
		Environment: pick("environment", "LOGGERAI_ENVIRONMENT", "production"),
		Host:        pick("host", "LOGGERAI_HOST", host),
		AllowHTTP:   pick("allow_http", "LOGGERAI_ALLOW_HTTP", "") == "true",
		Docker:      pick("docker", "LOGGERAI_DOCKER", "") == "true",
		DockerLogs:  pick("docker_logs", "LOGGERAI_DOCKER_LOGS", "") == "true",
		DockerSock:  pick("docker_socket", "LOGGERAI_DOCKER_SOCKET", ""),
		NginxStatus: pick("nginx_status", "LOGGERAI_NGINX_STATUS", ""),
		MySQLDSN:    pick("mysql_dsn", "LOGGERAI_MYSQL_DSN", ""),
	}
	if cfg.Host == "" {
		cfg.Host = "server"
	}
	if needKey {
		if err := cfg.validate(); err != nil {
			return Config{}, err
		}
	}
	return cfg, nil
}

func (c Config) validate() error {
	switch {
	case c.Key == "":
		return errors.New("no key: set key= in " + defaultConfig + " or LOGGERAI_KEY")
	case strings.HasPrefix(c.Key, "pk_"):
		return errors.New("that's a public key: the agent needs the project's secret key (sk_live_...)")
	case !strings.HasPrefix(c.Key, "sk_"):
		return errors.New("that doesn't look like a LoggerAI secret key (sk_live_...)")
	}
	u, err := url.Parse(c.Endpoint)
	if err != nil || u.Host == "" {
		return fmt.Errorf("the endpoint isn't a valid address: %q", c.Endpoint)
	}
	// The key travels in a header, so it must not cross a network unencrypted.
	local := u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1" || u.Hostname() == "::1"
	if u.Scheme != "https" && !(u.Scheme == "http" && (local || c.AllowHTTP)) {
		return errors.New("the endpoint must use https, so your key is encrypted on the way (allow_http=true overrides this on a private network)")
	}
	return nil
}
