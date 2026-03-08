package relay

import (
	"runtime"
	"sync/atomic"
)

// Metrics holds atomic counters for relay server statistics.
type Metrics struct {
	PacketsRouted      atomic.Uint64
	PacketsDropped     atomic.Uint64
	PacketsSpoofed     atomic.Uint64 // rejected by IP pinning
	PacketsRateLimited atomic.Uint64 // rejected by rate limiter
	BytesRouted        atomic.Uint64
	ActiveSessions     atomic.Int64
	ActivePlayers      atomic.Int64
}

// Snapshot returns a point-in-time copy of all metrics.
type MetricsSnapshot struct {
	PacketsRouted      uint64 `json:"packets_routed"`
	PacketsDropped     uint64 `json:"packets_dropped"`
	PacketsSpoofed     uint64 `json:"packets_spoofed"`
	PacketsRateLimited uint64 `json:"packets_rate_limited"`
	BytesRouted        uint64 `json:"bytes_routed"`
	ActiveSessions     int64  `json:"active_sessions"`
	ActivePlayers      int64  `json:"active_players"`
	// Goroutines is a live count of OS goroutines at snapshot time.
	// A sustained increase signals goroutine leaks (e.g. stalled TCP connections).
	Goroutines int64 `json:"goroutines"`
}

func (m *Metrics) Snapshot() MetricsSnapshot {
	return MetricsSnapshot{
		PacketsRouted:      m.PacketsRouted.Load(),
		PacketsDropped:     m.PacketsDropped.Load(),
		PacketsSpoofed:     m.PacketsSpoofed.Load(),
		PacketsRateLimited: m.PacketsRateLimited.Load(),
		BytesRouted:        m.BytesRouted.Load(),
		ActiveSessions:     m.ActiveSessions.Load(),
		ActivePlayers:      m.ActivePlayers.Load(),
		Goroutines:         int64(runtime.NumGoroutine()),
	}
}
