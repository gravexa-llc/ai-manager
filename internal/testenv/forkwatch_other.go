//go:build !darwin

package testenv

import "time"

// killStuck does nothing off darwin, where the bug it works around
// doesn't occur.
func killStuck(pid int, limit time.Duration, now time.Time) int { return 0 }
