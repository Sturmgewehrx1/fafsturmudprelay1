package relay

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"time"

	"fafsturmudprelay/internal/protocol"
)

// maxTCPFramePayload is the largest relay payload accepted from a TCP peer.
// Relay packets consist of an 8-byte header plus game data; game payloads are
// well under 1400 bytes.  Frames larger than this indicate a protocol error or
// a malicious sender — rejecting them early avoids reading garbage off the wire.
const maxTCPFramePayload = 1400

// tcpFramePool holds pre-allocated byte slices used to build outgoing TCP
// frames (2-byte length prefix + relay packet).  Reusing these buffers keeps
// writeTCPFrame allocation-free and avoids the scatter-gather overhead of
// net.Buffers.WriteTo, which on some platforms issues multiple syscalls.
var tcpFramePool = sync.Pool{
	New: func() interface{} {
		b := make([]byte, 2+maxTCPFramePayload)
		return &b
	},
}

const udpErrorBackoff = 100 * time.Millisecond

// Router is the hot-path UDP packet routing engine.
type Router struct {
	conn    *net.UDPConn
	sm      *SessionManager
	metrics *Metrics
	logger  *slog.Logger
}

// NewRouter creates a new packet router.
func NewRouter(conn *net.UDPConn, sm *SessionManager, metrics *Metrics, logger *slog.Logger) *Router {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Router{
		conn:    conn,
		sm:      sm,
		metrics: metrics,
		logger:  logger,
	}
}

// Run starts the packet routing loop. Blocks until context is cancelled.
func (r *Router) Run(ctx context.Context) {
	buf := make([]byte, 1500)

	for {
		// ReadFromUDPAddrPort returns netip.AddrPort by value — zero heap allocs.
		n, remoteAddr, err := r.conn.ReadFromUDPAddrPort(buf)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			r.logger.Error("UDP read error", "error", err)
			time.Sleep(udpErrorBackoff)
			continue
		}

		if n < protocol.RelayHeaderSize {
			r.metrics.PacketsDropped.Add(1)
			continue
		}

		senderID, destID, err := protocol.DecodeRelayHeader(buf[:n])
		if err != nil {
			r.metrics.PacketsDropped.Add(1)
			continue
		}

		session := r.sm.FindSessionByPlayer(senderID)
		if session == nil {
			r.metrics.PacketsDropped.Add(1)
			continue
		}

		// Update sender's address (NAT hole-punch learning)
		now := time.Now()
		verdict := session.UpdatePlayerAddr(senderID, remoteAddr, now)
		switch verdict {
		case VerdictSpoofed:
			r.metrics.PacketsSpoofed.Add(1)
			continue
		case VerdictRateLimited:
			r.metrics.PacketsRateLimited.Add(1)
			continue
		case VerdictUnknown:
			r.metrics.PacketsDropped.Add(1)
			continue
		case VerdictFirstAddr:
			r.logger.Info("Player connected via UDP", "player", senderID, "addr", remoteAddr, "transport", "udp")
		}

		// Route the packet
		if destID == senderID {
			// Hello/keepalive — echo back so the adapter can detect UDP reachability.
			// The adapter uses the echoed packet as a health-check "pong".
			r.conn.WriteToUDPAddrPort(buf[:n], remoteAddr)
			r.metrics.PacketsRouted.Add(1)
			continue
		}
		if destID == protocol.BroadcastDest {
			r.broadcast(session, senderID, buf[:n])
		} else {
			r.unicast(session, destID, buf[:n])
		}
	}
}

// sendToTarget routes a packet to a player via UDP or TCP depending on their transport.
func (r *Router) sendToTarget(t PlayerSendTarget, data []byte) bool {
	if t.TCPConn != nil {
		t.TCPConn.SetWriteDeadline(time.Now().Add(tcpWriteTimeout))
		if err := writeTCPFrame(t.TCPConn, data); err != nil {
			r.logger.Error("TCP write error", "dest", t.PlayerID, "error", err)
			return false
		}
		return true
	}
	if t.UDPAddr.IsValid() {
		_, err := r.conn.WriteToUDPAddrPort(data, t.UDPAddr)
		if err != nil {
			r.logger.Error("UDP write error", "dest", t.PlayerID, "error", err)
			return false
		}
		return true
	}
	return false
}

// unicast sends a packet to a single destination player within the same session.
// Cross-session routing is intentionally NOT supported to prevent packet leaks.
func (r *Router) unicast(session *Session, destID uint32, data []byte) {
	target, ok := session.GetPlayerTarget(destID)
	if !ok {
		r.metrics.PacketsDropped.Add(1)
		return
	}
	if !r.sendToTarget(target, data) {
		r.metrics.PacketsDropped.Add(1)
		return
	}
	r.metrics.PacketsRouted.Add(1)
	r.metrics.BytesRouted.Add(uint64(len(data)))
}

// broadcast sends a packet to all other players in the session.
func (r *Router) broadcast(session *Session, senderID uint32, data []byte) {
	session.ForEachOtherPlayerTarget(senderID, func(t PlayerSendTarget) {
		if !r.sendToTarget(t, data) {
			r.metrics.PacketsDropped.Add(1)
			return
		}
		r.metrics.PacketsRouted.Add(1)
		r.metrics.BytesRouted.Add(uint64(len(data)))
	})
}

// writeTCPFrame writes a length-prefixed packet to a TCP connection.
// Frame format: [2 bytes uint16 BE length][N bytes packet].
// Assembles header and data into a single pre-allocated buffer from tcpFramePool
// and issues one Write call, ensuring a single TCP segment regardless of
// platform scatter-gather support.
func writeTCPFrame(conn net.Conn, data []byte) error {
	total := 2 + len(data)
	bp := tcpFramePool.Get().(*[]byte)
	buf := (*bp)[:total]
	binary.BigEndian.PutUint16(buf[:2], uint16(len(data)))
	copy(buf[2:], data)
	_, err := conn.Write(buf)
	tcpFramePool.Put(bp)
	return err
}

// readTCPFrame reads a length-prefixed packet from a TCP connection into buf.
// Returns number of bytes read, or an error.
// Frames larger than maxTCPFramePayload are rejected immediately — the declared
// length is validated before attempting to read the body so the connection is
// closed rather than reading (and potentially allocating) arbitrarily large data.
func readTCPFrame(conn net.Conn, buf []byte) (int, error) {
	var hdr [2]byte
	if _, err := io.ReadFull(conn, hdr[:]); err != nil {
		return 0, err
	}
	n := int(binary.BigEndian.Uint16(hdr[:]))
	if n == 0 {
		return 0, fmt.Errorf("empty TCP frame")
	}
	if n > maxTCPFramePayload {
		return 0, fmt.Errorf("TCP frame too large: %d bytes (max %d)", n, maxTCPFramePayload)
	}
	if n > len(buf) {
		return 0, io.ErrShortBuffer
	}
	if _, err := io.ReadFull(conn, buf[:n]); err != nil {
		return 0, err
	}
	return n, nil
}

