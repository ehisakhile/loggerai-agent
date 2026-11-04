// The LoggerAI server agent: reads a Linux machine's CPU, memory, disks, network and busiest
// processes from /proc and sends them to LoggerAI once a minute.
//
// It only ever makes outbound HTTPS calls. It listens on no port, takes no commands from the
// server, and never reads process command lines, environment variables or file contents.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"
)

var version = "dev"

const (
	sampleEvery = 15 * time.Second
	reportEvery = 60 * time.Second
	shutdownFor = 3 * time.Second
)

const usage = `loggerai-agent %s

  loggerai-agent run      Run the agent (the service does this)
  loggerai-agent print    Show what would be sent, without sending it
  loggerai-agent check    Send one report, to test the key and connection
  loggerai-agent version  Show the version

Settings come from /etc/loggerai/agent.conf, or LOGGERAI_KEY, LOGGERAI_ENDPOINT,
LOGGERAI_ENVIRONMENT and LOGGERAI_HOST. LOGGERAI_CONFIG names another file.
`

func main() {
	log.SetFlags(0)
	cmd := "help"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	switch cmd {
	case "version", "--version":
		fmt.Println(version)
	case "print":
		cfg, err := loadConfig(false)
		fail(err)
		fail(printReport(cfg))
	case "check":
		cfg, err := loadConfig(true)
		fail(err)
		fail(check(cfg))
	case "run":
		cfg, err := loadConfig(true)
		fail(err)
		fail(run(cfg))
	default:
		fmt.Printf(usage, version)
		if cmd != "help" && cmd != "--help" && cmd != "-h" {
			os.Exit(2)
		}
	}
}

func fail(err error) {
	if err != nil {
		log.Fatalf("loggerai-agent: %v", err)
	}
}

// oneReport takes two readings a moment apart, since rates need a before and an after.
func oneReport(cfg Config) (Batch, error) {
	c := NewCollector(procRoot(), cfg.Host)
	if err := c.Sample(time.Now()); err != nil {
		return Batch{}, err
	}
	time.Sleep(2 * time.Second)
	if err := c.Sample(time.Now()); err != nil {
		return Batch{}, err
	}
	return c.Report(cfg, time.Now())
}

func printReport(cfg Config) error {
	b, err := oneReport(cfg)
	if err != nil {
		return err
	}
	out := json.NewEncoder(os.Stdout)
	out.SetIndent("", "  ")
	return out.Encode(b)
}

func check(cfg Config) error {
	b, err := oneReport(cfg)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if r := NewSender(cfg).Post(ctx, b); r.Err != nil {
		return r.Err
	}
	fmt.Printf("Sent a report for %s to %s. It should appear on the Servers page within a minute.\n", cfg.Host, cfg.Endpoint)
	return nil
}

func run(cfg Config) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	log.Printf("loggerai-agent %s reporting %s to %s", version, cfg.Host, cfg.Endpoint)

	c := NewCollector(procRoot(), cfg.Host)
	q := NewQueue(NewSender(cfg))
	if err := c.Sample(time.Now()); err != nil {
		return err
	}
	samples := time.NewTicker(sampleEvery)
	reports := time.NewTicker(reportEvery)
	defer samples.Stop()
	defer reports.Stop()
	for {
		select {
		case <-ctx.Done():
			last, cancel := context.WithTimeout(context.Background(), shutdownFor)
			defer cancel()
			if b, err := c.Report(cfg, time.Now()); err == nil {
				q.Add(b)
				q.Flush(last, time.Now())
			}
			return nil
		case now := <-samples.C:
			if err := c.Sample(now); err != nil {
				log.Printf("reading the machine failed: %v", err)
			}
		case now := <-reports.C:
			b, err := c.Report(cfg, now)
			if err != nil {
				log.Printf("building a report failed: %v", err)
				continue
			}
			q.Add(b)
			send, cancel := context.WithTimeout(ctx, 45*time.Second)
			q.Flush(send, now)
			cancel()
		}
	}
}
