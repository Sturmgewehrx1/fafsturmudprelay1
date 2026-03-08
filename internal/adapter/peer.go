package adapter

import (
	"context"
	"encoding/binary"
	"io"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

const udpErrorBackoff = 100 * time.Millisecond

// Echo frame protocol for end-to-end RTT measurement through the relay.
// Both frames are exactly echoFrameSize bytes long.
// Magic is 4 bytes — probability of game data matching: ~1/4 billion per packet.
var (
	echoMagicPing = [4]byte{0xAF, 0xEC, 0xFA, 0x00} // ping: us → remote
	echoMagicPong = [4]byte{0xAF, 0xED, 0xFA, 0x00} // pong: remote → us
)

const (
	echoFrameSize     = 12 // 4 magic + 8-byte nanosecond timestamp
	echoPingInterval  = 5 * time.Second
)

// Peer represents a per-peer bidirectional forwarder.
// Each peer has a local UDP socket that the game engine sends to.
// Packets from the game are forwarded to the relay; packets from
// the relay are sent to the game's learned address.
type Peer struct {
	PlayerID   int
	Login      string
	AddedOrder int    // insertion order assigned by PeerManager; used for stable debug-UI ordering
	LocalID    uint32 // our player ID

	conn     *net.UDPConn     // local UDP socket
	gameAddr *net.UDPAddr     // learned from first incoming game packet
	mu       sync.RWMutex
	relay    *RelayConnection
	fromCh   chan []byte       // channel for packets arriving from relay
	started  bool
	cancel   context.CancelFunc
	logger   *slog.Logger

	// End-to-end RTT measured via echo frames through the relay (atomic).
	rttMs atomic.Int64 // 0 = no measurement yet
}

// NewPeer creates a new peer with a local UDP socket.
func NewPeer(playerID int, login string, localID uint32, relay *RelayConnection, logger *slog.Logger) (*Peer, error) {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}

	// Bind local UDP socket for the game to communicate with this peer
	laddr, _ := net.ResolveUDPAddr("udp", "127.0.0.1:0")
	conn, err := net.ListenUDP("udp", laddr)
	if err != nil {
		return nil, err
	}

	return &Peer{
		PlayerID: playerID,
		Login:    login,
		LocalID:  localID,
		conn:     conn,
		relay:    relay,
		fromCh:   make(chan []byte, 256),
		logger:   logger,
	}, nil
}

// Start starts the bidirectional forwarding goroutines.
// Safe to call multiple times — only the first call starts goroutines.
func (p *Peer) Start(ctx context.Context) {
	p.mu.Lock()
	if p.started {
		p.mu.Unlock()
		return
	}
	p.started = true
	ctx, p.cancel = context.WithCancel(ctx)
	p.mu.Unlock()

	go p.gameToRelay(ctx)
	go p.relayToGame(ctx)
	go p.pingLoop(ctx)
}

// gameToRelay reads packets from the game engine via the local UDP socket
// and forwards them to the relay server.
func (p *Peer) gameToRelay(ctx context.Context) {
	buf := make([]byte, 1500)
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		n, remoteAddr, err := p.conn.ReadFromUDP(buf)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			p.logger.Error("peer local read error", "peer", p.PlayerID, "error", err)
			time.Sleep(udpErrorBackoff)
			continue
		}

		// Learn the game's address from the first packet
		p.mu.Lock()
		if p.gameAddr == nil {
			p.gameAddr = remoteAddr
			p.logger.Info("learned game address for peer", "peer", p.PlayerID, "addr", remoteAddr)
		}
		p.mu.Unlock()

		// Forward to relay: game data → relay server with peer's ID as dest
		if err := p.relay.Send(uint32(p.PlayerID), buf[:n]); err != nil {
			p.logger.Error("relay send error", "peer", p.PlayerID, "error", err)
		} else {
			p.logger.Debug("game→relay", "peer", p.PlayerID, "bytes", n)
		}
	}
}

// relayToGame reads packets from the relay (via channel) and sends them
// to the game engine's learned address.
// Echo ping/pong frames are intercepted here and never forwarded to the game.
func (p *Peer) relayToGame(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case data := <-p.fromCh:
			// Intercept echo frames (exactly echoFrameSize bytes with our magic).
			if len(data) == echoFrameSize {
				if [4]byte(data[:4]) == echoMagicPing {
					// Remote sent us a ping — echo it back as pong.
					pong := make([]byte, echoFrameSize)
					copy(pong[4:], data[4:]) // copy timestamp
					copy(pong[:4], echoMagicPong[:])
					if err := p.relay.Send(uint32(p.PlayerID), pong); err != nil {
						p.logger.Debug("echo pong send error", "peer", p.PlayerID, "error", err)
					}
					continue
				}
				if [4]byte(data[:4]) == echoMagicPong {
					// We received a pong for a ping we sent — measure RTT.
					sentNs := int64(binary.BigEndian.Uint64(data[4:]))
					if rtt := time.Now().UnixNano() - sentNs; rtt > 0 {
						p.rttMs.Store(rtt / int64(time.Millisecond))
					}
					continue
				}
			}

			p.mu.RLock()
			addr := p.gameAddr
			p.mu.RUnlock()

			if addr == nil {
				// Game hasn't sent a packet yet, so we don't know where to send
				p.logger.Warn("dropping relay→game packet, game address unknown", "peer", p.PlayerID)
				continue
			}

			if _, err := p.conn.WriteToUDP(data, addr); err != nil {
				p.logger.Error("peer local write error", "peer", p.PlayerID, "error", err)
			} else {
				p.logger.Debug("relay→game", "peer", p.PlayerID, "bytes", len(data), "dest", addr)
			}
		}
	}
}

// pingLoop sends periodic echo ping frames to the remote peer through the relay.
// The remote peer echoes them back; relayToGame measures the RTT on return.
func (p *Peer) pingLoop(ctx context.Context) {
	ticker := time.NewTicker(echoPingInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			frame := make([]byte, echoFrameSize)
			copy(frame[:4], echoMagicPing[:])
			binary.BigEndian.PutUint64(frame[4:], uint64(time.Now().UnixNano()))
			if err := p.relay.Send(uint32(p.PlayerID), frame); err != nil {
				p.logger.Debug("echo ping send error", "peer", p.PlayerID, "error", err)
			}
		}
	}
}

// DeliverFromRelay delivers a packet from the relay to this peer.
func (p *Peer) DeliverFromRelay(data []byte) {
	select {
	case p.fromCh <- data:
	default:
		p.logger.Warn("dropping relay packet, channel full", "peer", p.PlayerID)
	}
}

// LocalPort returns the local UDP port the game should connect to for this peer.
func (p *Peer) LocalPort() int {
	return p.conn.LocalAddr().(*net.UDPAddr).Port
}

// RttMs returns the end-to-end round-trip time through the relay in milliseconds.
// Uses echo-based measurement when available; falls back to relay server RTT.
// Returns 0 if no measurement is available yet.
func (p *Peer) RttMs() int64 {
	if ms := p.rttMs.Load(); ms > 0 {
		return ms
	}
	if p.relay == nil {
		return 0
	}
	return p.relay.RelayRttMs()
}

// RttIsEstimated returns true if RttMs is a fallback estimate (relay RTT only),
// false if it is a real end-to-end echo measurement.
func (p *Peer) RttIsEstimated() bool {
	return p.rttMs.Load() == 0
}

// Close stops the peer and closes its socket.
func (p *Peer) Close() {
	p.mu.Lock()
	cancel := p.cancel
	p.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	p.conn.Close()
}
