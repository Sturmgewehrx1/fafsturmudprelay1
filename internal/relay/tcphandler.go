package relay

import (
	"context"
	"log/slog"
	"net"
	"sync/atomic"
	"time"

	"fafsturmudprelay/internal/protocol"
)

const (
	// tcpReadTimeout is the max time between incoming TCP frames.
	// The adapter sends keepalive hellos every 15 s, so 60 s gives 4x headroom
	// before we consider the connection dead.  Protects against Slowloris-style
	// attacks where a client opens a TCP connection and sends nothing.
	tcpReadTimeout = 60 * time.Second

	// tcpWriteTimeout limits how long a single TCP write may block.
	// A stalled receiver (kernel send-buffer full) cannot hold up the routing
	// goroutine indefinitely — 10 s is generous enough for any healthy network.
	tcpWriteTimeout = 10 * time.Second

	// maxTCPConns is the maximum number of concurrently open TCP fallback
	// connections.  Each connection consumes a goroutine and a 1500-byte buffer.
	// 1000 covers the realistic player count with ample margin.
	maxTCPConns = 1000
)

// TCPHandler accepts TCP connections from clients that could not reach the relay
// via UDP (e.g. due to strict firewalls). Each TCP client sends and receives the
// same relay packet format as UDP clients, but wrapped in a 2-byte length prefix:
//
//	[2 bytes: uint16 BE packet length][N bytes: relay packet]
type TCPHandler struct {
	listener    net.Listener
	router      *Router // shared with the UDP router — not created per-connection
	sm          *SessionManager
	metrics     *Metrics
	logger      *slog.Logger
	activeConns atomic.Int32 // current number of open TCP connections
}

// NewTCPHandler creates a new TCP handler.
// The router is shared with the UDP routing loop so TCP connections reuse the
// same send path as UDP peers — no redundant struct allocation per connection.
func NewTCPHandler(listener net.Listener, router *Router, sm *SessionManager, metrics *Metrics, logger *slog.Logger) *TCPHandler {
	return &TCPHandler{
		listener: listener,
		router:   router,
		sm:       sm,
		metrics:  metrics,
		logger:   logger,
	}
}

// Run accepts incoming TCP connections until the context is cancelled.
func (h *TCPHandler) Run(ctx context.Context) {
	go func() {
		<-ctx.Done()
		h.listener.Close()
	}()

	for {
		conn, err := h.listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			h.logger.Error("TCP accept error", "error", err)
			continue
		}

		// Disable Nagle's algorithm: game packets are small and latency-sensitive.
		// Without TCP_NODELAY each hop may add up to ~40 ms (Nagle delay).
		if tc, ok := conn.(*net.TCPConn); ok {
			_ = tc.SetNoDelay(true)
		}

		// Connection limit: prevent goroutine/memory exhaustion under load.
		if h.activeConns.Load() >= maxTCPConns {
			h.logger.Warn("TCP connection limit reached, rejecting",
				"limit", maxTCPConns, "remote", conn.RemoteAddr())
			conn.Close()
			continue
		}
		h.activeConns.Add(1)
		go func() {
			defer h.activeConns.Add(-1)
			h.handleConn(ctx, conn)
		}()
	}
}

// handleConn processes all packets from a single TCP client connection.
func (h *TCPHandler) handleConn(ctx context.Context, conn net.Conn) {
	defer conn.Close()

	buf := make([]byte, 1500)

	for {
		if ctx.Err() != nil {
			return
		}

		// Renew read deadline before each frame.  If nothing arrives within
		// tcpReadTimeout the connection is considered dead and closed.
		conn.SetReadDeadline(time.Now().Add(tcpReadTimeout))

		n, err := readTCPFrame(conn, buf)
		if err != nil {
			if ctx.Err() == nil {
				h.logger.Debug("TCP client disconnected", "remote", conn.RemoteAddr(), "error", err)
			}
			return
		}

		if n < protocol.RelayHeaderSize {
			h.metrics.PacketsDropped.Add(1)
			continue
		}

		senderID, destID, err := protocol.DecodeRelayHeader(buf[:n])
		if err != nil {
			h.metrics.PacketsDropped.Add(1)
			continue
		}

		session := h.sm.FindSessionByPlayer(senderID)
		if session == nil {
			h.metrics.PacketsDropped.Add(1)
			continue
		}

		// Update sender's TCP connection (IP pinning + rate limit + first-time detection).
		// UpdatePlayerTCP returns the previous TCP conn so we can close it, which
		// terminates the goroutine that was serving the old connection.
		now := time.Now()
		verdict, oldConn := session.UpdatePlayerTCP(senderID, conn, now)

		// Close the old connection asynchronously to evict its goroutine.
		if oldConn != nil && oldConn != conn {
			go oldConn.Close()
		}

		switch verdict {
		case VerdictSpoofed:
			// Incoming connection is from a different IP than the pinned one —
			// reject the entire connection (not just the packet) since every
			// subsequent packet from this conn would also be spoofed.
			h.metrics.PacketsSpoofed.Add(1)
			return
		case VerdictRateLimited:
			h.metrics.PacketsRateLimited.Add(1)
			continue
		case VerdictUnknown:
			h.metrics.PacketsDropped.Add(1)
			continue
		case VerdictFirstAddr:
			h.logger.Info("Player connected via TCP", "player", senderID, "remote", conn.RemoteAddr())
		}

		// Hello/keepalive: echo back via TCP so adapter health monitor works
		if destID == senderID {
			conn.SetWriteDeadline(time.Now().Add(tcpWriteTimeout))
			if err := writeTCPFrame(conn, buf[:n]); err != nil {
				h.logger.Error("TCP echo error", "error", err)
				return
			}
			// Clear the write deadline so it does not poison future writes
			// from sendToTarget, which sets its own per-packet deadline.
			conn.SetWriteDeadline(time.Time{})
			h.metrics.PacketsRouted.Add(1)
			continue
		}

		// Route via shared router logic (handles both UDP and TCP destinations)
		data := buf[:n]
		if destID == protocol.BroadcastDest {
			h.router.broadcast(session, senderID, data)
		} else {
			h.router.unicast(session, destID, data)
		}
	}
}
