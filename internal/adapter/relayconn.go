package adapter

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"fafsturmudprelay/internal/protocol"
)

// tcpRelayReadTimeout is the maximum time the adapter will wait for a frame
// from the relay server over TCP.  The keepalive goroutine sends a hello
// every 15 s so 45 s gives 3× headroom before we treat the relay as dead.
const tcpRelayReadTimeout = 45 * time.Second

const (
	// HealthCheckInterval is how often the health monitor checks relay liveness.
	HealthCheckInterval = 15 * time.Second
	// HealthTimeout is how long without a pong before the relay is considered unreachable.
	HealthTimeout = 30 * time.Second
	// ProbeTimeout is the max time to wait for a hello echo during startup.
	ProbeTimeout = 5 * time.Second
)

// RelayConnection manages a connection to the relay server (UDP or TCP fallback).
type RelayConnection struct {
	// UDP transport (always created; may be unused in TCP mode)
	conn      *net.UDPConn
	relayAddr netip.AddrPort

	// TCP transport (non-nil when operating in TCP fallback mode)
	tcpConn     net.Conn
	tcpAddr     string // address used for initial TCP connect; used for reconnection
	tcpFrameBuf []byte // pre-allocated buffer for outgoing TCP frames (2-byte len + relay packet)
	useTCP      bool   // true = operate via tcpConn instead of conn

	localID uint32
	mu      sync.Mutex
	sendBuf []byte // pre-allocated send buffer (UDP), protected by mu
	peerMgr *PeerManager
	logger  *slog.Logger

	// Diagnostic counters (atomic, lock-free)
	pktsSent  atomic.Uint64
	pktsRecv  atomic.Uint64
	bytesSent atomic.Uint64
	bytesRecv atomic.Uint64

	// Health monitoring: updated by ReceiveLoop on every packet from relay.
	lastRecv    atomic.Int64 // unix nanoseconds of last received packet
	helloSentAt atomic.Int64 // unix nanoseconds when last hello was sent (for RTT)
	relayRttNs  atomic.Int64 // most recent relay round-trip time in nanoseconds

	// transportReady is closed once transport mode (UDP vs TCP) has been decided.
	// ReceiveLoop waits for this before starting to read, so it knows which
	// transport to use. nil means "already ready" (used by unit tests that
	// construct RelayConnection directly without NewRelayConnection).
	transportReady     chan struct{}
	transportReadyOnce sync.Once

	// relayConfirmed is closed once the relay server has been verified
	// reachable (first valid pong received). joinGame/connectToPeer wait
	// on this before telling the FAF client "Connected", so the client
	// stays on the "Connecting…" screen when the relay is down.
	// nil means "already confirmed" (unit tests that skip probing).
	relayConfirmed     chan struct{}
	relayConfirmedOnce sync.Once
}

// NewRelayConnection creates a new relay connection.
func NewRelayConnection(relayAddr string, localID uint32, peerMgr *PeerManager, logger *slog.Logger) (*RelayConnection, error) {
	raddr, err := net.ResolveUDPAddr("udp", relayAddr)
	if err != nil {
		return nil, err
	}

	// Bind local UDP socket
	laddr, _ := net.ResolveUDPAddr("udp", "0.0.0.0:0")
	conn, err := net.ListenUDP("udp", laddr)
	if err != nil {
		return nil, err
	}

	rc := &RelayConnection{
		conn:           conn,
		relayAddr:      raddr.AddrPort(),
		localID:        localID,
		sendBuf:        make([]byte, 1500),
		tcpFrameBuf:    make([]byte, 2+protocol.MaxRelayPacket),
		peerMgr:        peerMgr,
		logger:         logger,
		transportReady: make(chan struct{}),
		relayConfirmed: make(chan struct{}),
	}
	// lastRecv stays at zero until the first real pong arrives.
	// IsHealthy() treats zero as "never received" → false.
	return rc, nil
}

// MarkTransportReady signals that the transport mode (UDP vs TCP) has been
// decided. ReceiveLoop will not start reading until this is called.
// Safe to call multiple times — only the first call has any effect.
func (rc *RelayConnection) MarkTransportReady() {
	if rc.transportReady == nil {
		return
	}
	rc.transportReadyOnce.Do(func() { close(rc.transportReady) })
}

// confirmRelay marks the relay as confirmed reachable. Called on the first
// valid pong (echo) received from the relay server. Safe to call many times.
func (rc *RelayConnection) confirmRelay() {
	if rc.relayConfirmed == nil {
		return
	}
	rc.relayConfirmedOnce.Do(func() {
		rc.logger.Info("Relay server confirmed reachable (first pong received)")
		close(rc.relayConfirmed)
	})
}

// WaitRelayConfirmed blocks until the relay server has been confirmed reachable
// or ctx is cancelled. Returns true if confirmed, false if cancelled.
func (rc *RelayConnection) WaitRelayConfirmed(ctx context.Context) bool {
	if rc.relayConfirmed == nil {
		return true // nil = tests / direct construction, assume confirmed
	}
	select {
	case <-rc.relayConfirmed:
		return true
	case <-ctx.Done():
		return false
	}
}

// SwitchToTCP establishes a TCP connection to tcpAddr and switches the relay
// connection to TCP mode. All subsequent Send/Receive calls use the TCP conn.
// ctx is used to cancel the dial immediately when the adapter shuts down.
func (rc *RelayConnection) SwitchToTCP(ctx context.Context, tcpAddr string) error {
	dialCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	var d net.Dialer
	conn, err := d.DialContext(dialCtx, "tcp", tcpAddr)
	if err != nil {
		return fmt.Errorf("TCP connect to %s: %w", tcpAddr, err)
	}
	// Disable Nagle's algorithm: game packets are small and latency-sensitive.
	// Without TCP_NODELAY, Nagle may add up to ~40 ms per hop.
	if tc, ok := conn.(*net.TCPConn); ok {
		_ = tc.SetNoDelay(true)
	}
	rc.mu.Lock()
	rc.tcpConn = conn
	rc.tcpAddr = tcpAddr
	rc.useTCP = true
	rc.mu.Unlock()
	rc.logger.Info("Switched to TCP transport", "addr", tcpAddr)
	return nil
}

// Transport returns "tcp" or "udp" depending on the active transport.
func (rc *RelayConnection) Transport() string {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	if rc.useTCP {
		return "tcp"
	}
	return "udp"
}

// writeTCPFrame writes a length-prefixed packet to the TCP connection.
// Assembles the 2-byte frame header and data into the pre-allocated tcpFrameBuf
// and issues a single Write call — one syscall, one TCP segment.
// Must be called with rc.mu held.
func (rc *RelayConnection) writeTCPFrame(data []byte) error {
	total := 2 + len(data)
	binary.BigEndian.PutUint16(rc.tcpFrameBuf[:2], uint16(len(data)))
	copy(rc.tcpFrameBuf[2:total], data)
	_, err := rc.tcpConn.Write(rc.tcpFrameBuf[:total])
	return err
}

// readTCPFrame reads a length-prefixed packet from conn into buf.
// conn is passed explicitly so the caller can read from a specific (potentially
// reconnected) connection without holding the lock during the blocking read.
func readTCPFrame(conn net.Conn, buf []byte) (int, error) {
	var hdr [2]byte
	if _, err := io.ReadFull(conn, hdr[:]); err != nil {
		return 0, err
	}
	n := int(binary.BigEndian.Uint16(hdr[:]))
	if n > len(buf) {
		return 0, io.ErrShortBuffer
	}
	if _, err := io.ReadFull(conn, buf[:n]); err != nil {
		return 0, err
	}
	return n, nil
}

// Send sends a packet to the relay server with the appropriate header.
// In UDP mode reuses a pre-allocated buffer to avoid per-packet heap allocations.
func (rc *RelayConnection) Send(destID uint32, payload []byte) error {
	total := protocol.RelayHeaderSize + len(payload)

	// Enforce protocol maximum so the relay server never rejects our frames.
	// MaxRelayPacket (1400) < sendBuf (1500), so the slice below is always safe.
	if total > protocol.MaxRelayPacket {
		return fmt.Errorf("payload too large: %d bytes (max %d)", len(payload), protocol.MaxRelayPayload)
	}

	rc.mu.Lock()
	if rc.useTCP {
		// Build packet in sendBuf then send as TCP frame
		buf := rc.sendBuf[:total]
		protocol.EncodeRelayHeader(buf, rc.localID, destID)
		copy(buf[protocol.RelayHeaderSize:], payload)
		err := rc.writeTCPFrame(buf)
		rc.mu.Unlock()
		if err == nil {
			rc.pktsSent.Add(1)
			rc.bytesSent.Add(uint64(total))
		}
		return err
	}
	buf := rc.sendBuf[:total]
	protocol.EncodeRelayHeader(buf, rc.localID, destID)
	copy(buf[protocol.RelayHeaderSize:], payload)
	_, err := rc.conn.WriteToUDPAddrPort(buf, rc.relayAddr)
	rc.mu.Unlock()
	if err == nil {
		rc.pktsSent.Add(1)
		rc.bytesSent.Add(uint64(total))
	}
	return err
}

// SendHello sends a header-only packet to the relay server so it learns
// our address. The relay echoes it back as a health-check "pong".
func (rc *RelayConnection) SendHello() error {
	rc.helloSentAt.Store(time.Now().UnixNano())
	rc.mu.Lock()
	buf := rc.sendBuf[:protocol.RelayHeaderSize]
	protocol.EncodeRelayHeader(buf, rc.localID, rc.localID)
	if rc.useTCP {
		err := rc.writeTCPFrame(buf)
		rc.mu.Unlock()
		return err
	}
	_, err := rc.conn.WriteToUDPAddrPort(buf, rc.relayAddr)
	rc.mu.Unlock()
	return err
}

// ProbeRelayUDP sends hello packets to the relay server at 500 ms intervals
// and waits for an echoed pong to verify that the UDP path is working.
// Retransmitting ensures at least one hello arrives after the player is
// registered with the relay (registration runs concurrently and may take
// 1-3 s to complete; the server only echoes registered clients).
// Returns an error if no pong arrives within ProbeTimeout.
func (rc *RelayConnection) ProbeRelayUDP() error {
	buf := make([]byte, 1500)
	relayIP := rc.relayAddr.Addr()

	const retryInterval = 500 * time.Millisecond
	probeDeadline := time.Now().Add(ProbeTimeout)

	for {
		// Send hello (retransmit on each iteration)
		if err := rc.SendHello(); err != nil {
			return fmt.Errorf("sending probe: %w", err)
		}

		// Wait for a pong for up to retryInterval, then retry.
		waitUntil := time.Now().Add(retryInterval)
		if waitUntil.After(probeDeadline) {
			waitUntil = probeDeadline
		}
		rc.conn.SetReadDeadline(waitUntil)

		for {
			n, remoteAddr, err := rc.conn.ReadFromUDPAddrPort(buf)
			if err != nil {
				// Timeout — retry interval or overall deadline reached.
				break
			}
			if remoteAddr.Addr() != relayIP {
				continue
			}
			if n < protocol.RelayHeaderSize {
				continue
			}
			senderID, _, err := protocol.DecodeRelayHeader(buf[:n])
			if err != nil {
				continue
			}
			if senderID == rc.localID {
				rc.conn.SetReadDeadline(time.Time{})
				rc.lastRecv.Store(time.Now().UnixNano())
				rc.confirmRelay()
				rc.logger.Info("Relay UDP probe successful (pong received)", "transport", "udp")
				return nil
			}
		}

		// Check if overall deadline exceeded.
		if !time.Now().Before(probeDeadline) {
			rc.conn.SetReadDeadline(time.Time{})
			return fmt.Errorf("relay server did not respond within %v (server down or UDP blocked?)", ProbeTimeout)
		}
	}
}

// RunKeepalive sends periodic hello packets to keep the NAT mapping alive
// and ensure the relay server always has our current address.
// Blocks until context is cancelled.
func (rc *RelayConnection) RunKeepalive(ctx context.Context) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := rc.SendHello(); err != nil {
				rc.logger.Warn("keepalive send error", "error", err)
			}
		}
	}
}

// ReceiveLoop reads packets from the relay and dispatches to the correct peer.
// Blocks until context is cancelled. Automatically uses TCP or UDP mode.
//
// When transportReady is non-nil (i.e. when created via NewRelayConnection),
// the loop waits until MarkTransportReady() is called so that the transport
// mode (UDP vs TCP) is fully decided before the first read.
func (rc *RelayConnection) ReceiveLoop(ctx context.Context) {
	// Wait for transport mode to be decided (background probing may still be
	// in progress). Unit tests that construct RelayConnection directly set
	// transportReady to nil and skip this wait.
	if rc.transportReady != nil {
		select {
		case <-rc.transportReady:
		case <-ctx.Done():
			return
		}
	}

	rc.mu.Lock()
	isTCP := rc.useTCP
	rc.mu.Unlock()

	if isTCP {
		rc.receiveTCP(ctx)
	} else {
		rc.receiveUDP(ctx)
	}
}

// receiveUDP is the UDP receive path.
func (rc *RelayConnection) receiveUDP(ctx context.Context) {
	buf := make([]byte, 1500)
	relayIP := rc.relayAddr.Addr()

	for {
		// ReadFromUDPAddrPort returns netip.AddrPort by value — zero alloc.
		n, remoteAddr, err := rc.conn.ReadFromUDPAddrPort(buf)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			rc.logger.Error("relay receive error", "error", err)
			time.Sleep(udpErrorBackoff)
			continue
		}

		// Security: Only accept packets from the known relay server IP.
		if remoteAddr.Addr() != relayIP {
			continue
		}

		if n < protocol.RelayHeaderSize {
			continue
		}

		senderID, _, err := protocol.DecodeRelayHeader(buf[:n])
		if err != nil {
			continue
		}

		// Update health timestamp for every valid packet from relay
		rc.lastRecv.Store(time.Now().UnixNano())
		rc.confirmRelay()

		// Echoed hello (pong): senderID == localID — measure relay RTT
		if senderID == rc.localID {
			if sent := rc.helloSentAt.Load(); sent > 0 {
				rc.relayRttNs.Store(time.Now().UnixNano() - sent)
			}
			continue
		}

		rc.dispatchTopeer(senderID, buf[protocol.RelayHeaderSize:n], n)
	}
}

// reconnectTCP closes the broken TCP connection and dials a new one, retrying
// with exponential backoff until it succeeds or ctx is cancelled.
// On success it updates rc.tcpConn and sends a hello so the relay learns
// our presence again.
func (rc *RelayConnection) reconnectTCP(ctx context.Context) error {
	// Close the old broken connection to free its file descriptor.
	rc.mu.Lock()
	old := rc.tcpConn
	addr := rc.tcpAddr
	rc.mu.Unlock()
	if old != nil {
		old.Close()
	}

	const maxBackoff = 30 * time.Second
	backoff := 2 * time.Second

	for attempt := 1; ; attempt++ {
		rc.logger.Info("TCP reconnect attempt", "addr", addr, "attempt", attempt)

		dialCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		var d net.Dialer
		newConn, err := d.DialContext(dialCtx, "tcp", addr)
		cancel()

		if err == nil {
			if tc, ok := newConn.(*net.TCPConn); ok {
				_ = tc.SetNoDelay(true)
			}
			rc.mu.Lock()
			rc.tcpConn = newConn
			rc.mu.Unlock()
			// Send hello so the relay server re-learns our address.
			if helloErr := rc.SendHello(); helloErr != nil {
				rc.logger.Warn("Hello after TCP reconnect failed", "error", helloErr)
			}
			rc.logger.Info("TCP reconnected", "addr", addr, "attempt", attempt)
			return nil
		}

		if ctx.Err() != nil {
			return ctx.Err()
		}

		rc.logger.Warn("TCP reconnect failed, retrying",
			"error", err, "backoff", backoff)

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}

		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

// receiveTCP is the TCP receive path. It automatically reconnects when the
// TCP connection is lost, retrying with exponential backoff.
func (rc *RelayConnection) receiveTCP(ctx context.Context) {
	buf := make([]byte, 1500)

	for {
		if ctx.Err() != nil {
			return
		}

		// Capture the current connection for this read iteration.
		// After a reconnection the outer loop re-captures the new one.
		rc.mu.Lock()
		conn := rc.tcpConn
		rc.mu.Unlock()

		// Inner read loop: read frames until a hard error occurs.
		for {
			if ctx.Err() != nil {
				return
			}

			// Renew read deadline before each frame. The keepalive goroutine
			// sends a hello every 15 s so echoes arrive regularly; 45 s gives
			// 3× headroom before we treat the link as dead.
			conn.SetReadDeadline(time.Now().Add(tcpRelayReadTimeout))

			n, err := readTCPFrame(conn, buf)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				// Soft timeout: relay silent but connection still alive.
				var netErr net.Error
				if errors.As(err, &netErr) && netErr.Timeout() {
					rc.logger.Debug("relay TCP read timeout, waiting for keepalive echo")
					continue
				}
				// Hard error: connection broken — exit inner loop to reconnect.
				rc.logger.Error("relay TCP connection lost", "error", err)
				break
			}

			if n < protocol.RelayHeaderSize {
				continue
			}

			senderID, _, err := protocol.DecodeRelayHeader(buf[:n])
			if err != nil {
				continue
			}

			// Update health timestamp
			rc.lastRecv.Store(time.Now().UnixNano())
			rc.confirmRelay()

			// Echoed hello (pong): senderID == localID — measure relay RTT
			if senderID == rc.localID {
				if sent := rc.helloSentAt.Load(); sent > 0 {
					rc.relayRttNs.Store(time.Now().UnixNano() - sent)
				}
				continue
			}

			rc.dispatchTopeer(senderID, buf[protocol.RelayHeaderSize:n], n)
		}

		// TCP connection lost — attempt reconnection.
		if err := rc.reconnectTCP(ctx); err != nil {
			return // context cancelled during reconnect
		}
		rc.logger.Info("TCP reconnected, resuming receive loop")
	}
}

// dispatchTopeer delivers a received packet to the appropriate peer.
func (rc *RelayConnection) dispatchTopeer(senderID uint32, payload []byte, totalBytes int) {
	peer := rc.peerMgr.GetPeer(int(senderID))
	if peer == nil {
		rc.logger.Debug("received packet from unknown peer (not yet registered via ConnectToPeer)", "sender", senderID)
		return
	}

	// Copy payload for the channel (buf is reused next iteration)
	data := make([]byte, len(payload))
	copy(data, payload)

	rc.pktsRecv.Add(1)
	rc.bytesRecv.Add(uint64(totalBytes))
	peer.DeliverFromRelay(data)
}

// RunHealthMonitor periodically checks if the relay is still reachable by
// verifying that echoed hello (pong) packets are being received.
// Calls onStateChange when the relay transitions between reachable/unreachable.
// Blocks until context is cancelled.
func (rc *RelayConnection) RunHealthMonitor(ctx context.Context, onStateChange func(reachable bool)) {
	ticker := time.NewTicker(HealthCheckInterval)
	defer ticker.Stop()

	wasReachable := false // start as unreachable; first pong flips to true

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			lastNano := rc.lastRecv.Load()
			reachable := true
			if lastNano == 0 {
				// Never received anything
				reachable = false
			} else {
				elapsed := time.Since(time.Unix(0, lastNano))
				reachable = elapsed < HealthTimeout
			}

			if reachable != wasReachable {
				if reachable {
					rc.logger.Info("Relay server is reachable again")
				} else {
					rc.logger.Warn("Relay server unreachable (no pong received)",
						"timeout", HealthTimeout)
				}
				onStateChange(reachable)
				wasReachable = reachable
			}
		}
	}
}

// RelayRttMs returns the most recent relay server round-trip time in milliseconds.
// Returns 0 if no measurement is available yet.
func (rc *RelayConnection) RelayRttMs() int64 {
	ns := rc.relayRttNs.Load()
	if ns <= 0 {
		return 0
	}
	return ns / int64(time.Millisecond)
}

// IsHealthy returns true if a packet was received from the relay within HealthTimeout.
func (rc *RelayConnection) IsHealthy() bool {
	lastNano := rc.lastRecv.Load()
	if lastNano == 0 {
		return false
	}
	return time.Since(time.Unix(0, lastNano)) < HealthTimeout
}

// RunStats logs packet rate statistics periodically.
// Blocks until context is cancelled.
func (rc *RelayConnection) RunStats(ctx context.Context) {
	const interval = 10 * time.Second
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	var prevSent, prevRecv, prevBytesSent, prevBytesRecv uint64

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sent := rc.pktsSent.Load()
			recv := rc.pktsRecv.Load()
			bs := rc.bytesSent.Load()
			br := rc.bytesRecv.Load()

			dSent := sent - prevSent
			dRecv := recv - prevRecv
			dBS := bs - prevBytesSent
			dBR := br - prevBytesRecv

			prevSent, prevRecv = sent, recv
			prevBytesSent, prevBytesRecv = bs, br

			// Only log when there's traffic
			if dSent > 0 || dRecv > 0 {
				rc.logger.Info("relay stats",
					"sent_pps", dSent*uint64(time.Second)/uint64(interval),
					"recv_pps", dRecv*uint64(time.Second)/uint64(interval),
					"sent_kbps", dBS*8*uint64(time.Second)/uint64(interval)/1000,
					"recv_kbps", dBR*8*uint64(time.Second)/uint64(interval)/1000,
					"total_sent", sent,
					"total_recv", recv,
				)
			}
		}
	}
}

// Close closes the relay connection (both UDP socket and TCP conn if open).
func (rc *RelayConnection) Close() error {
	rc.mu.Lock()
	tc := rc.tcpConn
	rc.mu.Unlock()
	if tc != nil {
		tc.Close()
	}
	return rc.conn.Close()
}

// LocalAddr returns the local address of the relay UDP socket.
func (rc *RelayConnection) LocalAddr() net.Addr {
	return rc.conn.LocalAddr()
}
