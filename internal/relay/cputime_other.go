//go:build !windows

package relay

import "time"

// processCPUTime is a stub for non-Windows platforms.
func processCPUTime() time.Duration {
	return 0
}
