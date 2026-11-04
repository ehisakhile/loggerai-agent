package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func write(t *testing.T, root, name, text string) {
	t.Helper()
	p := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func fakeProc(t *testing.T) string {
	root := t.TempDir()
	write(t, root, "stat", "cpu  100 0 100 800 0 0 0 0 0 0\ncpu0 50 0 50 400 0 0 0 0 0 0\ncpu1 50 0 50 400 0 0 0 0 0 0\nbtime 1700000000\n")
	write(t, root, "meminfo", "MemTotal: 2048000 kB\nMemFree: 100000 kB\nMemAvailable: 1024000 kB\nSwapTotal: 1000 kB\nSwapFree: 750 kB\n")
	write(t, root, "loadavg", "0.50 0.25 0.10 1/100 1234\n")
	write(t, root, "net/dev", "Inter-|   Receive\n face |bytes\n    lo: 999 1 0 0 0 0 0 0 999 1 0 0 0 0 0 0\n  eth0: 1000 1 0 0 0 0 0 0 2000 1 0 0 0 0 0 0\n")
	write(t, root, "diskstats", "   8 0 sda 1 0 10 0 1 0 20 0 0 0 0\n   8 1 sda1 1 0 10 0 1 0 20 0 0 0 0\n")
	write(t, root, "mounts", "/dev/sda1 / ext4 rw 0 0\n")
	write(t, root, "sys/kernel/osrelease", "6.8.0\n")
	write(t, root, "10/stat", procStat("nginx", 100, 1000))
	write(t, root, "11/stat", procStat("nginx", 100, 1000))
	return root
}

func procStat(name string, ticks, rss int) string {
	f := make([]string, 22)
	for i := range f {
		f[i] = "0"
	}
	f[0] = "S"
	f[11] = itoa(ticks)
	f[21] = itoa(rss)
	return "10 (" + name + ") " + strings.Join(f, " ") + " 0 0\n"
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	s := ""
	for ; n > 0; n /= 10 {
		s = string(rune('0'+n%10)) + s
	}
	return s
}

func TestCPUFromTheChangeBetweenReadings(t *testing.T) {
	root := fakeProc(t)
	c := NewCollector(root, "web-1")
	at := time.Unix(1000, 0)
	if err := c.Sample(at); err != nil {
		t.Fatal(err)
	}
	if len(c.readings) != 0 {
		t.Fatal("the first reading has nothing to compare with, so it can't report CPU")
	}
	write(t, root, "stat", "cpu  175 0 100 825 0 0 0 0 0 0\ncpu0 1\ncpu1 1\n")
	if err := c.Sample(at.Add(15 * time.Second)); err != nil {
		t.Fatal(err)
	}
	h := c.readings[0]
	if h.CPUPct == nil || *h.CPUPct != 75 {
		t.Fatalf("75 busy of 100 ticks is 75%%, got %v", h.CPUPct)
	}
	if h.CPUs != 2 || h.MemUsedPct != 50 || h.MemTotalMB != 2000 || h.Load1 != 0.5 || *h.SwapPct != 25 {
		t.Fatalf("readings: %+v", h)
	}
}

func TestIOWaitIsNotCountedAsBusy(t *testing.T) {
	root := fakeProc(t)
	c := NewCollector(root, "h")
	c.Sample(time.Now())
	write(t, root, "stat", "cpu  100 0 100 800 100 0 0 0 0 0\n")
	c.Sample(time.Now())
	if *c.readings[0].CPUPct != 0 {
		t.Fatalf("waiting on disk isn't using the CPU: %v", *c.readings[0].CPUPct)
	}
}

func TestProcessNamesCanHoldSpacesAndBrackets(t *testing.T) {
	name, ticks, rss, ok := parseProcStat([]byte(procStat("Web Content) (x", 42, 7)))
	if !ok || name != "Web Content) (x" || ticks != 42 || rss != 7 {
		t.Fatalf("%q %d %d %v", name, ticks, rss, ok)
	}
	if _, _, _, ok := parseProcStat([]byte("garbage")); ok {
		t.Fatal("garbage parsed")
	}
}

func TestProcessesGroupByNameAndShareOfMachine(t *testing.T) {
	root := fakeProc(t)
	c := NewCollector(root, "h")
	cfg := Config{Environment: "production"}
	if _, err := c.Report(cfg, time.Unix(1000, 0)); err != nil {
		t.Fatal(err)
	}
	write(t, root, "stat", "cpu  200 0 200 1000 0 0 0 0 0 0\n") // 400 ticks pass
	write(t, root, "10/stat", procStat("nginx", 200, 1000))     // 100 of them here
	write(t, root, "11/stat", procStat("nginx", 200, 1000))     // and 100 here
	b, err := c.Report(cfg, time.Unix(1060, 0))
	if err != nil {
		t.Fatal(err)
	}
	p := b.Servers[0].Processes
	if len(p) != 1 || p[0].Name != "nginx" || p[0].Count != 2 || p[0].CPUPct != 50 {
		t.Fatalf("%+v", p)
	}
}

func TestAReusedPidDoesNotCountOldTicks(t *testing.T) {
	root := fakeProc(t)
	c := NewCollector(root, "h")
	c.Report(Config{}, time.Unix(1000, 0))
	write(t, root, "stat", "cpu  200 0 200 1000 0 0 0 0 0 0\n")
	write(t, root, "11/stat", procStat("backup", 5000, 10))
	b, _ := c.Report(Config{}, time.Unix(1060, 0))
	for _, p := range b.Servers[0].Processes {
		if p.Name == "backup" && p.CPUPct != 0 {
			t.Fatalf("a new process was given another's history: %+v", p)
		}
	}
}

func TestTopKeepsTheBiggestMemoryUserEvenIfIdle(t *testing.T) {
	g := map[string]*Process{"hog": {Name: "hog", MemMB: 9000}}
	for i := 0; i < 20; i++ {
		n := "busy" + itoa(i+1)
		g[n] = &Process{Name: n, CPUPct: 1, MemMB: 1}
	}
	out := topOf(g)
	found := false
	for _, p := range out {
		found = found || p.Name == "hog"
	}
	if !found || len(out) != topProcesses+5 {
		t.Fatalf("%d processes, hog found: %v", len(out), found)
	}
}

func TestRatesAndCounterResets(t *testing.T) {
	root := fakeProc(t)
	c := NewCollector(root, "h")
	c.Report(Config{}, time.Unix(1000, 0))
	write(t, root, "net/dev", "  eth0: 7000 1 0 0 0 0 0 0 2600 1 0 0 0 0 0 0\n")
	write(t, root, "diskstats", "   8 0 sda 1 0 130 0 1 0 20 0 0 0 0\n")
	b, _ := c.Report(Config{}, time.Unix(1060, 0))
	s := b.Servers[0]
	if s.NetInBps != 100 || s.NetOutBps != 10 || s.DiskReadBps != 1024 || s.DiskWriteBps != 0 {
		t.Fatalf("%+v", s)
	}
	write(t, root, "net/dev", "  eth0: 5 1 0 0 0 0 0 0 5 1 0 0 0 0 0 0\n") // a reboot
	b, _ = c.Report(Config{}, time.Unix(1120, 0))
	if b.Servers[0].NetInBps != 0 {
		t.Fatalf("a reset counter isn't a huge negative rate: %v", b.Servers[0].NetInBps)
	}
}

func TestVirtualInterfacesAndPartitionsAreSkipped(t *testing.T) {
	in, _ := parseNetDev([]byte("lo: 5 0 0 0 0 0 0 0 5\ndocker0: 5 0 0 0 0 0 0 0 5\nveth12: 5 0 0 0 0 0 0 0 5\nens5: 9 0 0 0 0 0 0 0 5\n"))
	if in != 9 {
		t.Fatal(in)
	}
	r, _ := parseDiskstats([]byte("259 0 nvme0n1 1 0 2 0 1 0 2 0 0 0 0\n259 1 nvme0n1p1 1 0 2 0 1 0 2 0 0 0 0\n7 0 loop0 1 0 2 0 1 0 2 0 0 0 0\n"))
	if r != 1024 {
		t.Fatal(r)
	}
}

func TestOnlyRealLocalDisksAreChecked(t *testing.T) {
	got := parseMounts([]byte(`overlay / overlay rw 0 0
proc /proc proc rw 0 0
tmpfs /run tmpfs rw 0 0
/dev/sda2 /data\040disk ext4 rw 0 0
/dev/sda2 /bind ext4 rw 0 0
/dev/sdb1 /etc/hostname ext4 rw 0 0
/dev/loop1 /snap/core/1 squashfs ro 0 0
nas:/share /mnt/nas nfs4 rw 0 0
`))
	if len(got) != 2 || got[0].path != "/" || got[1].path != "/data disk" {
		t.Fatalf("%+v", got)
	}
}

func TestMemoryOnOldKernelsAndNoSwap(t *testing.T) {
	_, used, swap := memory(map[string]uint64{"MemTotal": 1000, "MemFree": 100, "Buffers": 100, "Cached": 300})
	if used != 50 || swap != nil {
		t.Fatal(used, swap)
	}
}

func TestConfig(t *testing.T) {
	for name, c := range map[string]Config{
		"public key":     {Key: "pk_live_x", Endpoint: "https://a.example/x"},
		"no key":         {Endpoint: "https://a.example/x"},
		"plain http":     {Key: "sk_live_x", Endpoint: "http://a.example/x"},
		"not an address": {Key: "sk_live_x", Endpoint: "nonsense"},
	} {
		if c.validate() == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	if err := (Config{Key: "sk_live_x", Endpoint: "http://127.0.0.1:8077/x"}).validate(); err != nil {
		t.Error(err)
	}
	p := filepath.Join(t.TempDir(), "agent.conf")
	os.WriteFile(p, []byte("# a comment\nkey = \"sk_live_abc\"\nENVIRONMENT=staging\nnot a setting\n"), 0o640)
	t.Setenv("LOGGERAI_CONFIG", p)
	t.Setenv("LOGGERAI_HOST", "from-env")
	c, err := loadConfig(true)
	if err != nil || c.Key != "sk_live_abc" || c.Environment != "staging" || c.Host != "from-env" {
		t.Fatalf("%+v %v", c, err)
	}
}

func TestRedirectsNeverCarryTheKey(t *testing.T) {
	leaked := false
	evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leaked = r.Header.Get("x-loggerai-key") != ""
	}))
	defer evil.Close()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, evil.URL, http.StatusTemporaryRedirect)
	}))
	defer api.Close()
	r := NewSender(Config{Key: "sk_live_x", Endpoint: api.URL}).Post(context.Background(), Batch{})
	if leaked || r.Err == nil {
		t.Fatalf("leaked=%v err=%v", leaked, r.Err)
	}
}

func TestSenderKinds(t *testing.T) {
	status := 202
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-loggerai-key") != "sk_live_x" || r.Header.Get("content-encoding") != "gzip" {
			t.Error("headers")
		}
		w.WriteHeader(status)
	}))
	defer api.Close()
	s := NewSender(Config{Key: "sk_live_x", Endpoint: api.URL})
	for code, want := range map[int]Result{202: {}, 401: {Retry: true, Auth: true}, 503: {Retry: true}, 429: {Retry: true}, 413: {}} {
		status = code
		r := s.Post(context.Background(), Batch{})
		if (r.Err != nil) != (code != 202) || r.Retry != want.Retry || r.Auth != want.Auth {
			t.Errorf("%d: %+v", code, r)
		}
	}
}

type script struct {
	results []Result
	got     []string
}

func (s *script) Post(_ context.Context, b Batch) Result {
	s.got = append(s.got, b.Host)
	if len(s.results) == 0 {
		return Result{}
	}
	r := s.results[0]
	s.results = s.results[1:]
	return r
}

func TestQueueKeepsReportsThroughAnOutageInOrder(t *testing.T) {
	down := Result{Err: errors.New("down"), Retry: true}
	to := &script{results: []Result{down}}
	q := NewQueue(to)
	at := time.Unix(1000, 0)
	q.Add(Batch{Host: "a"})
	q.Flush(context.Background(), at)
	q.Add(Batch{Host: "b"})
	q.Flush(context.Background(), at.Add(10*time.Second)) // still backing off
	if len(to.got) != 1 || len(q.items) != 2 {
		t.Fatalf("%v %d", to.got, len(q.items))
	}
	q.Flush(context.Background(), at.Add(2*time.Minute))
	if strings.Join(to.got, "") != "aab" || len(q.items) != 0 {
		t.Fatalf("%v %d", to.got, len(q.items))
	}
}

func TestQueueDropsTheOldestWhenFullAndSkipsWhatCanNeverSend(t *testing.T) {
	q := NewQueue(&script{})
	for i := 0; i < queueMax+5; i++ {
		q.Add(Batch{Host: itoa(i + 1)})
	}
	if len(q.items) != queueMax || q.items[0].Host != "6" {
		t.Fatalf("%d %s", len(q.items), q.items[0].Host)
	}
	to := &script{results: []Result{{Err: errors.New("413")}}}
	q = NewQueue(to)
	q.Add(Batch{Host: "bad"})
	q.Add(Batch{Host: "good"})
	q.Flush(context.Background(), time.Unix(1, 0))
	if len(q.items) != 0 || len(to.got) != 2 {
		t.Fatalf("%v", to.got)
	}
}

func TestARefusedKeyBacksOffAtOnce(t *testing.T) {
	q := NewQueue(&script{results: []Result{{Err: errors.New("401"), Retry: true, Auth: true}}})
	q.Add(Batch{})
	now := time.Unix(1000, 0)
	q.Flush(context.Background(), now)
	if q.next != now.Add(backoffMax) {
		t.Fatal(q.next)
	}
}
