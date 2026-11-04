//go:build linux

package main

import "syscall"

func statDisk(m mount) (Disk, bool) {
	var s syscall.Statfs_t
	if syscall.Statfs(m.path, &s) != nil || s.Blocks == 0 {
		return Disk{}, false
	}
	size := uint64(s.Bsize)
	used := s.Blocks - s.Bfree
	// As df works it out: space kept back for root doesn't count as available.
	usable := used + s.Bavail
	d := Disk{Mount: m.path, FS: m.fs, TotalGB: round2(float64(s.Blocks*size) / 1e9)}
	if usable > 0 {
		d.UsedPct = round2(float64(used) / float64(usable) * 100)
	}
	if s.Files > 0 {
		d.InodesPct = round2(float64(s.Files-s.Ffree) / float64(s.Files) * 100)
	}
	return d, true
}
