//go:build !linux

package main

// The agent runs on Linux. This lets the tests run on a developer's own machine.
func statDisk(m mount) (Disk, bool) { return Disk{}, false }
