package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

const (
	queueMax   = 60 // an hour of reports, kept while LoggerAI can't be reached
	sendPerRun = 20 // catching up after an outage goes gently
	backoffMax = 5 * time.Minute
)

type Result struct {
	Err   error
	Retry bool // worth sending again: the network, or LoggerAI, may recover
	Auth  bool // the key was refused
}

type Sender struct {
	client   *http.Client
	endpoint string
	key      string
}

func NewSender(cfg Config) *Sender {
	return &Sender{
		endpoint: cfg.Endpoint,
		key:      cfg.Key,
		client: &http.Client{
			Timeout: 15 * time.Second,
			// Redirects would carry the key header to wherever they point, so none are followed.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

func (s *Sender) Post(ctx context.Context, b Batch) Result {
	var body bytes.Buffer
	z := gzip.NewWriter(&body)
	if err := json.NewEncoder(z).Encode(b); err != nil {
		return Result{Err: err}
	}
	if err := z.Close(); err != nil {
		return Result{Err: err}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.endpoint, &body)
	if err != nil {
		return Result{Err: err}
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("content-encoding", "gzip")
	req.Header.Set("x-loggerai-key", s.key)
	req.Header.Set("user-agent", "loggerai-agent/"+version)
	res, err := s.client.Do(req)
	if err != nil {
		return Result{Err: fmt.Errorf("couldn't reach LoggerAI: %w", err), Retry: true}
	}
	defer res.Body.Close()
	text, _ := io.ReadAll(io.LimitReader(res.Body, 500))
	why := strings.TrimSpace(string(text))
	switch {
	case res.StatusCode >= 200 && res.StatusCode < 300:
		return Result{}
	case res.StatusCode == 401 || res.StatusCode == 403:
		return Result{Err: fmt.Errorf("LoggerAI refused the key (%d): check it's the project's current secret key", res.StatusCode), Retry: true, Auth: true}
	case res.StatusCode == 429 || res.StatusCode >= 500 || (res.StatusCode >= 300 && res.StatusCode < 400):
		return Result{Err: fmt.Errorf("LoggerAI answered %d", res.StatusCode), Retry: true}
	}
	return Result{Err: fmt.Errorf("LoggerAI refused a report (%d): %s", res.StatusCode, why)}
}

type poster interface {
	Post(context.Context, Batch) Result
}

// Queue holds reports until LoggerAI has them, oldest first, and drops the oldest when full.
type Queue struct {
	to      poster
	items   []Batch
	wait    time.Duration
	next    time.Time
	lastLog string
}

func NewQueue(to poster) *Queue { return &Queue{to: to} }

func (q *Queue) Add(b Batch) {
	q.items = append(q.items, b)
	if len(q.items) > queueMax {
		q.items = q.items[len(q.items)-queueMax:]
	}
}

func (q *Queue) Flush(ctx context.Context, now time.Time) {
	if now.Before(q.next) {
		return
	}
	for sent := 0; len(q.items) > 0 && sent < sendPerRun; sent++ {
		r := q.to.Post(ctx, q.items[0])
		if r.Err != nil && r.Retry {
			q.failed(r, now)
			return
		}
		if r.Err != nil {
			log.Print(r.Err) // this one report will never be accepted: don't let it block the rest
		} else if q.wait > 0 {
			log.Print("reconnected to LoggerAI")
		}
		q.wait, q.next, q.lastLog = 0, time.Time{}, ""
		q.items = q.items[1:]
	}
}

func (q *Queue) failed(r Result, now time.Time) {
	q.wait = min(max(q.wait*2, time.Minute), backoffMax)
	if r.Auth {
		q.wait = backoffMax // it won't fix itself in a minute
	}
	q.next = now.Add(q.wait)
	if msg := r.Err.Error(); msg != q.lastLog { // one line per problem, not one a minute
		log.Printf("%s. Keeping %d report(s) and trying again.", msg, len(q.items))
		q.lastLog = msg
	}
}
