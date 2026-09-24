package convert

import "time"

// SetElapsedClock replaces the per-file timing clock and returns a restore func.
func SetElapsedClock(f func() time.Time) (restore func()) {
	prev := elapsedClock
	elapsedClock = f
	return func() { elapsedClock = prev }
}
