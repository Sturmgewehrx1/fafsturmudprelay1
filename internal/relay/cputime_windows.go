package relay

import (
	"syscall"
	"time"
)

// processCPUTime returns the total CPU time (kernel + user) of the current process.
func processCPUTime() time.Duration {
	h, err := syscall.GetCurrentProcess()
	if err != nil {
		return 0
	}
	var creation, exit, kernel, user syscall.Filetime
	err = syscall.GetProcessTimes(h, &creation, &exit, &kernel, &user)
	if err != nil {
		return 0
	}
	// Filetime is in 100-nanosecond intervals
	kNano := (int64(kernel.HighDateTime)<<32 | int64(kernel.LowDateTime)) * 100
	uNano := (int64(user.HighDateTime)<<32 | int64(user.LowDateTime)) * 100
	return time.Duration(kNano + uNano)
}
