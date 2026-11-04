package main

import (
	"bufio"
	"bytes"
	"errors"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	topProcesses = 10
	maxDisks     = 20
	maxReadings  = 8 // samples kept between reports, if a send runs long
)

// What a batch looks like: docs/INGEST.md in the API. "hosts" feeds the same charts and alerts as
// the SDKs, and "servers" adds what only an agent can see.
type Batch struct {
	Environment string   `json:"environment"`
	Service     string   `json:"service"`
	Host        string   `json:"host"`
	Hosts       []Host   `json:"hosts"`
	Servers     []Server `json:"servers"`
}

type Host struct {
	TS         float64  `json:"ts"`
	Host       string   `json:"host"`
	CPUPct     *float64 `json:"cpu_pct,omitempty"`
	CPUs       int      `json:"cpus"`
	Load1      float64  `json:"load1"`
	MemUsedPct float64  `json:"mem_used_pct"`
	MemTotalMB float64  `json:"mem_total_mb"`
	SwapPct    *float64 `json:"swap_used_pct,omitempty"`
	DiskPct    *float64 `json:"disk_used_pct,omitempty"`
}

type Server struct {
	TS           float64   `json:"ts"`
	Host         string    `json:"host"`
	OS           string    `json:"os"`
	Kernel       string    `json:"kernel"`
	Arch         string    `json:"arch"`
	AgentVersion string    `json:"agent_version"`
	CPUs         int       `json:"cpus"`
	MemTotalMB   float64   `json:"mem_total_mb"`
	BootTime     int64     `json:"boot_time"`
	Load5        float64   `json:"load5"`
	Load15       float64   `json:"load15"`
	IOWaitPct    float64   `json:"iowait_pct"`
	StealPct     float64   `json:"steal_pct"`
	NetInBps     float64   `json:"net_in_bps"`
	NetOutBps    float64   `json:"net_out_bps"`
	DiskReadBps  float64   `json:"disk_read_bps"`
	DiskWriteBps float64   `json:"disk_write_bps"`
	Disks        []Disk    `json:"disks"`
	Processes    []Process `json:"processes"`
}

type Disk struct {
	Mount     string  `json:"mount"`
	FS        string  `json:"fs"`
	TotalGB   float64 `json:"total_gb"`
	UsedPct   float64 `json:"used_pct"`
	InodesPct float64 `json:"inodes_pct"`
}

// Process is every process with one name, added together: twelve php-fpm workers are one line.
type Process struct {
	Name   string  `json:"name"`
	Count  int     `json:"count"`
	CPUPct float64 `json:"cpu_pct"`
	MemMB  float64 `json:"mem_mb"`
}

type cpuTimes struct{ total, idle, iowait, steal uint64 }

type counters struct {
	at                           time.Time
	netIn, netOut, dRead, dWrite uint64
}

type Collector struct {
	root      string
	host      string
	cpu       *cpuTimes
	readings  []Host
	lastCount *counters
	lastSlice *cpuTimes         // CPU times at the last report
	lastProc  map[int]procTicks // and each process's
}

type procTicks struct {
	name  string
	ticks uint64
}

func NewCollector(root, host string) *Collector {
	return &Collector{root: root, host: host}
}

func (c *Collector) read(name string) ([]byte, error) {
	return os.ReadFile(filepath.Join(c.root, name))
}

// ---- CPU, memory, load -------------------------------------------------------------------

func parseCPU(stat []byte) (cpuTimes, int, error) {
	var t cpuTimes
	cpus, found := 0, false
	lines := bufio.NewScanner(bytes.NewReader(stat))
	lines.Buffer(make([]byte, 0, 64*1024), 4*1024*1024) // the "intr" line can be very long
	for lines.Scan() {
		f := strings.Fields(lines.Text())
		if len(f) == 0 || !strings.HasPrefix(f[0], "cpu") {
			continue
		}
		if f[0] != "cpu" {
			cpus++
			continue
		}
		if len(f) < 5 {
			return t, 0, errors.New("short cpu line in stat")
		}
		found = true
		// user nice system idle iowait irq softirq steal. Guest time is already inside user.
		v := make([]uint64, 8)
		for i := 0; i < 8 && i+1 < len(f); i++ {
			v[i], _ = strconv.ParseUint(f[i+1], 10, 64)
			t.total += v[i]
		}
		t.idle, t.iowait, t.steal = v[3], v[4], v[7]
	}
	if !found {
		return t, 0, errors.New("no cpu line in stat")
	}
	return t, cpus, lines.Err()
}

func parseMeminfo(b []byte) map[string]uint64 {
	out := map[string]uint64{}
	lines := bufio.NewScanner(bytes.NewReader(b))
	for lines.Scan() {
		k, v, ok := strings.Cut(lines.Text(), ":")
		if !ok {
			continue
		}
		f := strings.Fields(v)
		if len(f) > 0 {
			out[k], _ = strconv.ParseUint(f[0], 10, 64)
		}
	}
	return out
}

func memory(m map[string]uint64) (totalMB, usedPct float64, swapPct *float64) {
	total := m["MemTotal"]
	if total == 0 {
		return 0, 0, nil
	}
	free, ok := m["MemAvailable"]
	if !ok { // kernels before 3.14
		free = m["MemFree"] + m["Buffers"] + m["Cached"]
	}
	if free > total {
		free = total
	}
	usedPct = round2(float64(total-free) / float64(total) * 100)
	if s := m["SwapTotal"]; s > 0 {
		p := round2(float64(s-min(m["SwapFree"], s)) / float64(s) * 100)
		swapPct = &p
	}
	return round2(float64(total) / 1024), usedPct, swapPct
}

func parseLoad(b []byte) (l1, l5, l15 float64) {
	f := strings.Fields(string(b))
	if len(f) >= 3 {
		l1, _ = strconv.ParseFloat(f[0], 64)
		l5, _ = strconv.ParseFloat(f[1], 64)
		l15, _ = strconv.ParseFloat(f[2], 64)
	}
	return
}

func share(part, total uint64) float64 {
	if total == 0 {
		return 0
	}
	return round2(float64(part) / float64(total) * 100)
}

func sub(a, b uint64) uint64 { // counters reset on reboot and can wrap
	if a < b {
		return 0
	}
	return a - b
}

// Sample takes the quick readings: every 15 seconds, so a short spike still shows.
func (c *Collector) Sample(now time.Time) error {
	stat, err := c.read("stat")
	if err != nil {
		return err
	}
	t, cpus, err := parseCPU(stat)
	if err != nil {
		return err
	}
	mem, err := c.read("meminfo")
	if err != nil {
		return err
	}
	load, _ := c.read("loadavg")
	total, used, swap := memory(parseMeminfo(mem))
	l1, _, _ := parseLoad(load)
	h := Host{TS: unix(now), Host: c.host, CPUs: cpus, Load1: l1, MemUsedPct: used, MemTotalMB: total, SwapPct: swap}
	if c.cpu != nil {
		if d := sub(t.total, c.cpu.total); d > 0 {
			idle := sub(t.idle, c.cpu.idle) + sub(t.iowait, c.cpu.iowait)
			p := share(sub(d, idle), d)
			h.CPUPct = &p
		}
	}
	c.cpu = &t
	if h.CPUPct != nil { // the first reading has nothing to compare with
		c.readings = append(c.readings, h)
		if len(c.readings) > maxReadings {
			c.readings = c.readings[len(c.readings)-maxReadings:]
		}
	}
	return nil
}

// ---- network and disk throughput ---------------------------------------------------------

var virtualNet = regexp.MustCompile(`^(lo|veth|docker|br-|virbr|cali|flannel|cni|tun|tap|dummy|kube)`)

func parseNetDev(b []byte) (in, out uint64) {
	lines := bufio.NewScanner(bytes.NewReader(b))
	for lines.Scan() {
		name, rest, ok := strings.Cut(lines.Text(), ":")
		name = strings.TrimSpace(name)
		if !ok || virtualNet.MatchString(name) {
			continue
		}
		f := strings.Fields(rest)
		if len(f) < 9 {
			continue
		}
		a, _ := strconv.ParseUint(f[0], 10, 64)
		b, _ := strconv.ParseUint(f[8], 10, 64)
		in += a
		out += b
	}
	return
}

// Whole disks only: counting sda and sda1 would count every byte twice.
var wholeDisk = regexp.MustCompile(`^((sd|vd|xvd|hd)[a-z]+|nvme\d+n\d+|mmcblk\d+)$`)

func parseDiskstats(b []byte) (read, written uint64) {
	lines := bufio.NewScanner(bytes.NewReader(b))
	for lines.Scan() {
		f := strings.Fields(lines.Text())
		if len(f) < 10 || !wholeDisk.MatchString(f[2]) {
			continue
		}
		r, _ := strconv.ParseUint(f[5], 10, 64)
		w, _ := strconv.ParseUint(f[9], 10, 64)
		read += r * 512 // sectors here are always 512 bytes, whatever the disk's real size
		written += w * 512
	}
	return
}

// ---- disks -------------------------------------------------------------------------------

// Local filesystems only. A network mount that has gone away would hang a statfs call for ever.
var localFS = map[string]bool{"ext2": true, "ext3": true, "ext4": true, "xfs": true, "btrfs": true, "zfs": true, "f2fs": true, "vfat": true, "exfat": true, "ntfs": true, "ntfs3": true, "jfs": true, "reiserfs": true, "bcachefs": true}

// Snap images, Docker's own layers, and single files a container has mounted over /etc.
var skipMount = regexp.MustCompile(`^/(snap|var/lib/docker|etc)/`)

type mount struct{ dev, path, fs string }

func unescapeMount(s string) string { // /proc/mounts writes a space as \040
	return strings.NewReplacer(`\040`, " ", `\011`, "\t", `\134`, `\`).Replace(s)
}

func parseMounts(b []byte) []mount {
	var out []mount
	seen := map[string]bool{}
	lines := bufio.NewScanner(bytes.NewReader(b))
	for lines.Scan() {
		f := strings.Fields(lines.Text())
		if len(f) < 3 {
			continue
		}
		m := mount{f[0], unescapeMount(f[1]), f[2]}
		if m.path != "/" && (!localFS[m.fs] || skipMount.MatchString(m.path)) {
			continue
		}
		if seen[m.dev] { // bind mounts and btrfs subvolumes: one disk, listed once
			continue
		}
		seen[m.dev] = true
		out = append(out, m)
	}
	return out
}

func (c *Collector) disks() ([]Disk, *float64) {
	b, err := c.read("mounts")
	if err != nil {
		return []Disk{}, nil
	}
	out := []Disk{}
	var worst *float64
	for _, m := range parseMounts(b) {
		d, ok := statDisk(m)
		if !ok {
			continue
		}
		out = append(out, d)
		if worst == nil || d.UsedPct > *worst {
			p := d.UsedPct
			worst = &p
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UsedPct > out[j].UsedPct })
	if len(out) > maxDisks {
		out = out[:maxDisks]
	}
	return out, worst
}

// ---- processes ---------------------------------------------------------------------------

// parseProcStat reads /proc/<pid>/stat. The name sits in brackets and may itself hold spaces and
// brackets, so everything else is counted from the last ")".
func parseProcStat(b []byte) (name string, ticks, rssPages uint64, ok bool) {
	open, end := bytes.IndexByte(b, '('), bytes.LastIndexByte(b, ')')
	if open < 0 || end < open {
		return "", 0, 0, false
	}
	f := strings.Fields(string(b[end+1:]))
	if len(f) < 22 { // f[0] is field 3 of proc(5): state
		return "", 0, 0, false
	}
	u, _ := strconv.ParseUint(f[11], 10, 64)
	s, _ := strconv.ParseUint(f[12], 10, 64)
	r, _ := strconv.ParseInt(f[21], 10, 64)
	if r < 0 {
		r = 0
	}
	return string(b[open+1 : end]), u + s, uint64(r), true
}

func (c *Collector) processes(now cpuTimes) []Process {
	entries, err := os.ReadDir(c.root)
	if err != nil {
		return []Process{}
	}
	var machine uint64
	if c.lastSlice != nil {
		machine = sub(now.total, c.lastSlice.total)
	}
	page := float64(os.Getpagesize())
	seen := make(map[int]procTicks, len(entries))
	groups := map[string]*Process{}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		b, err := c.read(filepath.Join(e.Name(), "stat"))
		if err != nil {
			continue // it has already exited
		}
		name, ticks, rss, ok := parseProcStat(b)
		if !ok {
			continue
		}
		seen[pid] = procTicks{name, ticks}
		if rss == 0 && ticks == 0 {
			continue
		}
		g := groups[name]
		if g == nil {
			g = &Process{Name: name}
			groups[name] = g
		}
		g.Count++
		g.MemMB += float64(rss) * page / 1048576
		// A pid reused by another program isn't the same process, so it starts from nothing.
		if last, ok := c.lastProc[pid]; ok && last.name == name && machine > 0 {
			g.CPUPct += float64(sub(ticks, last.ticks)) / float64(machine) * 100
		}
	}
	c.lastProc = seen
	return topOf(groups)
}

// The busiest by CPU, then the largest by memory: a quiet process can still be the one eating RAM.
func topOf(groups map[string]*Process) []Process {
	all := make([]Process, 0, len(groups))
	for _, g := range groups {
		g.CPUPct, g.MemMB = round2(g.CPUPct), round2(g.MemMB)
		all = append(all, *g)
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].CPUPct != all[j].CPUPct {
			return all[i].CPUPct > all[j].CPUPct
		}
		return all[i].Name < all[j].Name
	})
	out := all[:min(topProcesses, len(all))]
	rest := append([]Process{}, all[len(out):]...)
	sort.Slice(rest, func(i, j int) bool { return rest[i].MemMB > rest[j].MemMB })
	return append(append([]Process{}, out...), rest[:min(5, len(rest))]...)
}

// ---- the machine itself ------------------------------------------------------------------

func osName() string {
	for _, p := range []string{"/etc/os-release", "/usr/lib/os-release"} {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		for _, l := range strings.Split(string(b), "\n") {
			if v, ok := strings.CutPrefix(l, "PRETTY_NAME="); ok {
				return strings.Trim(v, `"'`)
			}
		}
	}
	return runtime.GOOS
}

func bootTime(stat []byte) int64 {
	for _, l := range strings.Split(string(stat), "\n") {
		if v, ok := strings.CutPrefix(l, "btime "); ok {
			n, _ := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
			return n
		}
	}
	return 0
}

// ---- a report ----------------------------------------------------------------------------

// Report is everything since the last one: the 15-second readings, plus one summary.
func (c *Collector) Report(cfg Config, now time.Time) (Batch, error) {
	stat, err := c.read("stat")
	if err != nil {
		return Batch{}, err
	}
	t, cpus, err := parseCPU(stat)
	if err != nil {
		return Batch{}, err
	}
	mem, err := c.read("meminfo")
	if err != nil {
		return Batch{}, err
	}
	load, _ := c.read("loadavg")
	_, l5, l15 := parseLoad(load)
	total, _, _ := memory(parseMeminfo(mem))
	kernel, _ := c.read("sys/kernel/osrelease")
	disks, worst := c.disks()
	s := Server{
		TS: unix(now), Host: c.host, OS: osName(), Kernel: strings.TrimSpace(string(kernel)), Arch: runtime.GOARCH, AgentVersion: version,
		CPUs: cpus, MemTotalMB: total, BootTime: bootTime(stat), Load5: l5, Load15: l15, Disks: disks, Processes: c.processes(t),
	}
	if c.lastSlice != nil {
		d := sub(t.total, c.lastSlice.total)
		s.IOWaitPct = share(sub(t.iowait, c.lastSlice.iowait), d)
		s.StealPct = share(sub(t.steal, c.lastSlice.steal), d)
	}
	c.lastSlice = &t

	netb, _ := c.read("net/dev")
	diskb, _ := c.read("diskstats")
	in, out := parseNetDev(netb)
	dr, dw := parseDiskstats(diskb)
	cur := &counters{now, in, out, dr, dw}
	if last := c.lastCount; last != nil {
		if secs := now.Sub(last.at).Seconds(); secs > 0 {
			rate := func(a, b uint64) float64 { return math.Round(float64(sub(a, b)) / secs) }
			s.NetInBps, s.NetOutBps = rate(in, last.netIn), rate(out, last.netOut)
			s.DiskReadBps, s.DiskWriteBps = rate(dr, last.dRead), rate(dw, last.dWrite)
		}
	}
	c.lastCount = cur

	hosts := c.readings
	c.readings = nil
	for i := range hosts {
		hosts[i].DiskPct = worst
	}
	if hosts == nil {
		hosts = []Host{}
	}
	return Batch{Environment: cfg.Environment, Service: "agent", Host: c.host, Hosts: hosts, Servers: []Server{s}}, nil
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }

func unix(t time.Time) float64 { return float64(t.UnixMilli()) / 1000 }
