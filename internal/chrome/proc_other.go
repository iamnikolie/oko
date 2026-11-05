//go:build !darwin && !linux

package chrome

import "time"

// osIdentity: without a way to prove a process's identity, oko owns none and
// signals none (browsers still close over CDP).
func osIdentity(pid int) (time.Time, string, string, error) {
	return time.Time{}, "", "", errNoIdentity
}
