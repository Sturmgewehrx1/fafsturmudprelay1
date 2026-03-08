package relay

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"runtime"
	"sync"
	"time"
)

const (
	// PlayerTimeout is how long a player can be silent before being reaped.
	// Reduced from 5 min to 90 s — inactive players consume index slots and
	// allow broadcast amplification; 90 s is ample for reconnect scenarios.
	PlayerTimeout = 90 * time.Second
	// SessionEmptyTimeout is how long an empty session persists before removal.
	SessionEmptyTimeout = 2 * time.Minute
	// ReaperInterval is how often the reaper runs.
	ReaperInterval = 30 * time.Second
)

const (
	// MaxPacketsPerSecond is the per-player packet rate limit.
	// FA typically sends ~30 PPS per peer. With 31 peers (32-player game)
	// that is ~930 PPS. 2000 gives headroom for bursts and reconnects.
	// Matches the kernel-level DDoS protection limit.
	MaxPacketsPerSecond = 2000
)

// PlayerInfo holds per-player state within a session.
type PlayerInfo struct {
	PlayerID  uint32
	Login     string
	Addr      netip.AddrPort // last known UDP address (learned from incoming packets)
	TCPConn   net.Conn       // TCP connection (nil if player uses UDP)
	tcpIP     net.IP         // pinned TCP remote IP (set on first TCP packet; prevents hijacking)
	Transport string         // "udp" or "tcp"
	LastSeen  time.Time

	// Rate limiting: packets this second + the second they belong to.
	// Both accessed under Session.mu — no extra sync needed.
	ratePkts   uint32
	rateSecond int64 // unix seconds
}

// CheckRate returns true if the player is within rate limit.
// Must be called with s.mu held (write lock).
func (p *PlayerInfo) CheckRate(nowUnix int64) bool {
	if p.rateSecond != nowUnix {
		p.rateSecond = nowUnix
		p.ratePkts = 0
	}
	p.ratePkts++
	return p.ratePkts <= MaxPacketsPerSecond
}

// Session represents a game session on the relay server.
type Session struct {
	ID         string
	MaxPlayers int
	CreatedAt  time.Time
	mu         sync.RWMutex
	players    map[uint32]*PlayerInfo // playerID → PlayerInfo
}

// NewSession creates a new session.
func NewSession(id string, maxPlayers int) *Session {
	return &Session{
		ID:         id,
		MaxPlayers: maxPlayers,
		CreatedAt:  time.Now(),
		players:    make(map[uint32]*PlayerInfo),
	}
}

// AddPlayer adds a player to the session.
func (s *Session) AddPlayer(playerID uint32, login string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.players[playerID]; exists {
		return fmt.Errorf("player %d already in session %s", playerID, s.ID)
	}
	if len(s.players) >= s.MaxPlayers {
		return fmt.Errorf("session %s full (%d/%d)", s.ID, len(s.players), s.MaxPlayers)
	}
	s.players[playerID] = &PlayerInfo{
		PlayerID: playerID,
		Login:    login,
	}
	return nil
}

// ResetPlayerTransport clears a player's transport state (address, TCP conn,
// IP pin) so that the next incoming packet re-learns the address from scratch.
// Used when a player re-joins the same session after a disconnect — the new
// adapter process has a different UDP port and possibly a different IP.
func (s *Session) ResetPlayerTransport(playerID uint32) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.players[playerID]
	if !ok {
		return
	}
	p.Addr = netip.AddrPort{}
	p.TCPConn = nil
	p.tcpIP = nil
	p.Transport = ""
	p.LastSeen = time.Time{}
}

// RemovePlayer removes a player from the session.
// Returns true if the player existed and was removed, false if not found.
func (s *Session) RemovePlayer(playerID uint32) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, existed := s.players[playerID]
	delete(s.players, playerID)
	return existed
}

// GetPlayer returns a player's info, or nil if not found.
func (s *Session) GetPlayer(playerID uint32) *PlayerInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.players[playerID]
}

// GetPlayerAddr returns a player's address without pointer allocation.
func (s *Session) GetPlayerAddr(playerID uint32) (netip.AddrPort, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.players[playerID]
	if !ok {
		return netip.AddrPort{}, false
	}
	return p.Addr, true
}

// PacketVerdict indicates why a packet was accepted or rejected.
type PacketVerdict int

const (
	VerdictOK          PacketVerdict = iota
	VerdictFirstAddr                 // first packet — address learned
	VerdictUnknown                   // unknown player
	VerdictSpoofed                   // IP doesn't match pinned address
	VerdictRateLimited               // exceeds per-player rate limit
)

// UpdatePlayerAddr updates a player's last known UDP address and checks rate limit.
// Security: Once a player's IP is learned from the first packet, only
// packets from the same IP are accepted. Port changes (NAT remapping)
// are allowed. This prevents sender-ID spoofing attacks where an
// attacker sends packets with a victim's playerID to hijack their session.
// Also enforces per-player rate limiting (MaxPacketsPerSecond).
// The caller passes now to avoid redundant time.Now() calls in the hot path.
func (s *Session) UpdatePlayerAddr(playerID uint32, addr netip.AddrPort, now time.Time) PacketVerdict {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.players[playerID]
	if !ok {
		return VerdictUnknown
	}
	if p.Addr.IsValid() && p.Addr.Addr() != addr.Addr() {
		return VerdictSpoofed
	}
	if !p.CheckRate(now.Unix()) {
		return VerdictRateLimited
	}
	wasNew := !p.Addr.IsValid() && p.TCPConn == nil
	p.Addr = addr
	// Clear any stale TCP state: the player is now using UDP.
	// Without this, sendToTarget would try the closed TCP conn first.
	if p.TCPConn != nil {
		p.TCPConn = nil
		p.tcpIP = nil
	}
	p.Transport = "udp"
	p.LastSeen = now
	if wasNew {
		return VerdictFirstAddr
	}
	return VerdictOK
}

// ForEachOtherPlayerAddr iterates over all other players' addresses under
// the read lock, calling fn for each player that has a valid address.
// Zero allocations on the hot path.
func (s *Session) ForEachOtherPlayerAddr(excludeID uint32, fn func(playerID uint32, addr netip.AddrPort)) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for id, p := range s.players {
		if id != excludeID && p.Addr.IsValid() {
			fn(id, p.Addr)
		}
	}
}

// PlayerSendTarget holds the routing destination for a player (UDP or TCP).
type PlayerSendTarget struct {
	PlayerID uint32
	UDPAddr  netip.AddrPort
	TCPConn  net.Conn
}

// ForEachOtherPlayerTarget iterates over all other players, returning both
// UDP addr and TCP conn so the router can pick the right transport.
func (s *Session) ForEachOtherPlayerTarget(excludeID uint32, fn func(t PlayerSendTarget)) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for id, p := range s.players {
		if id != excludeID {
			if p.TCPConn != nil || p.Addr.IsValid() {
				fn(PlayerSendTarget{PlayerID: id, UDPAddr: p.Addr, TCPConn: p.TCPConn})
			}
		}
	}
}

// GetPlayerTarget returns the routing destination for a single player.
func (s *Session) GetPlayerTarget(playerID uint32) (PlayerSendTarget, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.players[playerID]
	if !ok {
		return PlayerSendTarget{}, false
	}
	return PlayerSendTarget{PlayerID: playerID, UDPAddr: p.Addr, TCPConn: p.TCPConn}, true
}

// tcpConnIP extracts the IP address from a net.Conn's remote address.
func tcpConnIP(conn net.Conn) net.IP {
	if conn == nil {
		return nil
	}
	addr := conn.RemoteAddr()
	if addr == nil {
		return nil
	}
	host, _, err := net.SplitHostPort(addr.String())
	if err != nil {
		return nil
	}
	return net.ParseIP(host)
}

// UpdatePlayerTCP stores a player's TCP connection and updates activity time.
// Returns the packet verdict and the previous TCP connection (if any) so the
// caller can close it — this evicts stale goroutines on reconnect.
//
// Security: Once a player's TCP IP is learned from the first TCP packet, only
// connections from the same IP are accepted. This mirrors the UDP IP-pinning
// and prevents an attacker who knows a player's ID from hijacking their session
// by opening a new TCP connection.
func (s *Session) UpdatePlayerTCP(playerID uint32, conn net.Conn, now time.Time) (PacketVerdict, net.Conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.players[playerID]
	if !ok {
		return VerdictUnknown, nil
	}
	// TCP IP pinning: reject connections from a different IP than the pinned one.
	connIP := tcpConnIP(conn)
	if p.tcpIP != nil && !p.tcpIP.Equal(connIP) {
		return VerdictSpoofed, nil
	}
	if !p.CheckRate(now.Unix()) {
		return VerdictRateLimited, nil
	}
	wasNew := p.TCPConn == nil && !p.Addr.IsValid()
	oldConn := p.TCPConn // may be non-nil when player reconnects via TCP
	p.TCPConn = conn
	if p.tcpIP == nil && connIP != nil {
		p.tcpIP = connIP
	}
	p.Transport = "tcp"
	p.LastSeen = now
	if wasNew {
		return VerdictFirstAddr, oldConn
	}
	return VerdictOK, oldConn
}

// GetOtherPlayers returns all players except the given one.
func (s *Session) GetOtherPlayers(excludeID uint32) []*PlayerInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]*PlayerInfo, 0, len(s.players)-1)
	for id, p := range s.players {
		if id != excludeID {
			result = append(result, p)
		}
	}
	return result
}

// PlayerCount returns the number of players in the session.
func (s *Session) PlayerCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.players)
}

// PlayerIDs returns a slice of all player IDs.
func (s *Session) PlayerIDs() []uint32 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ids := make([]uint32, 0, len(s.players))
	for id := range s.players {
		ids = append(ids, id)
	}
	return ids
}

const (
	// MaxTotalSessions is the global limit on concurrent sessions.
	MaxTotalSessions = 15000
	// MaxTotalPlayers is the global limit on concurrent players across all sessions.
	MaxTotalPlayers = 50000
)

// SessionManager manages sessions and provides O(1) player lookup.
type SessionManager struct {
	mu          sync.RWMutex
	sessions    map[string]*Session  // sessionID → Session
	playerIndex map[uint32]string    // playerID → sessionID (for O(1) routing)
	metrics     *Metrics
}

// NewSessionManager creates a new session manager.
func NewSessionManager(metrics *Metrics) *SessionManager {
	return &SessionManager{
		sessions:    make(map[string]*Session),
		playerIndex: make(map[uint32]string),
		metrics:     metrics,
	}
}

// CreateSession creates a new session.
func (sm *SessionManager) CreateSession(id string, maxPlayers int) (*Session, error) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	if _, exists := sm.sessions[id]; exists {
		return nil, fmt.Errorf("session %s already exists", id)
	}
	if len(sm.sessions) >= MaxTotalSessions {
		return nil, fmt.Errorf("server at session capacity (%d)", MaxTotalSessions)
	}
	s := NewSession(id, maxPlayers)
	sm.sessions[id] = s
	sm.metrics.ActiveSessions.Add(1)
	return s, nil
}

// GetSession returns a session by ID.
func (sm *SessionManager) GetSession(id string) *Session {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return sm.sessions[id]
}

// DeleteSession removes a session and all its player index entries.
func (sm *SessionManager) DeleteSession(id string) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	s, exists := sm.sessions[id]
	if !exists {
		return fmt.Errorf("session %s not found", id)
	}
	// Remove all players from the index
	playerIDs := s.PlayerIDs()
	for _, pid := range playerIDs {
		delete(sm.playerIndex, pid)
	}
	sm.metrics.ActivePlayers.Add(-int64(len(playerIDs)))
	delete(sm.sessions, id)
	sm.metrics.ActiveSessions.Add(-1)
	return nil
}

// AddPlayerToSession adds a player to a session and updates the index.
// If the player is already in a different session, they are migrated
// (removed from the old session and added to the new one). This handles
// the common case where the FAF client relaunches the adapter with a new
// game-id while the player is still registered in the previous session.
// If the player is already in the target session, this is a no-op (idempotent).
func (sm *SessionManager) AddPlayerToSession(sessionID string, playerID uint32, login string) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	s, exists := sm.sessions[sessionID]
	if !exists {
		return fmt.Errorf("session %s not found", sessionID)
	}
	// Handle player already registered somewhere
	if existingSessionID, ok := sm.playerIndex[playerID]; ok {
		if existingSessionID == sessionID {
			// Already in this session — reset transport state so a
			// reconnecting adapter (new process, new UDP port) is not
			// routed via stale addresses or closed TCP connections.
			s.ResetPlayerTransport(playerID)
			return nil
		}
		// Migrate: remove from old session, then add to new one
		if oldSession, exists := sm.sessions[existingSessionID]; exists {
			oldSession.RemovePlayer(playerID)
		}
		delete(sm.playerIndex, playerID)
		sm.metrics.ActivePlayers.Add(-1)
	}
	if len(sm.playerIndex) >= MaxTotalPlayers {
		return fmt.Errorf("server at player capacity (%d)", MaxTotalPlayers)
	}
	if err := s.AddPlayer(playerID, login); err != nil {
		return err
	}
	sm.playerIndex[playerID] = sessionID
	sm.metrics.ActivePlayers.Add(1)
	return nil
}

// RemovePlayerFromSession removes a player from a session and the index.
// Returns an error if the session doesn't exist or the player is not in it.
func (sm *SessionManager) RemovePlayerFromSession(sessionID string, playerID uint32) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	s, exists := sm.sessions[sessionID]
	if !exists {
		return fmt.Errorf("session %s not found", sessionID)
	}
	if !s.RemovePlayer(playerID) {
		return fmt.Errorf("player %d not in session %s", playerID, sessionID)
	}
	delete(sm.playerIndex, playerID)
	sm.metrics.ActivePlayers.Add(-1)
	return nil
}

// FindSessionByPlayer looks up a player's session. O(1) via index.
func (sm *SessionManager) FindSessionByPlayer(playerID uint32) *Session {
	sm.mu.RLock()
	sessionID, ok := sm.playerIndex[playerID]
	if !ok {
		sm.mu.RUnlock()
		return nil
	}
	s := sm.sessions[sessionID]
	sm.mu.RUnlock()
	return s
}

// StalePlayers returns IDs of players who haven't sent a packet within timeout.
// Players who never sent a packet (Addr invalid, LastSeen zero) are considered stale
// if they joined more than timeout ago.
func (s *Session) StalePlayers(timeout time.Duration) []uint32 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	now := time.Now()
	var stale []uint32
	for id, p := range s.players {
		if p.LastSeen.IsZero() {
			// Never sent a packet — stale if session is old enough
			if now.Sub(s.CreatedAt) > timeout {
				stale = append(stale, id)
			}
		} else if now.Sub(p.LastSeen) > timeout {
			stale = append(stale, id)
		}
	}
	return stale
}

// RemovePlayerIfStale removes a player only if they are still stale at call
// time. Returns (true, tcpConn) if removed so the caller can close the conn.
// Must NOT be called with sm.mu held (acquires s.mu.Lock internally).
func (s *Session) RemovePlayerIfStale(playerID uint32, timeout time.Duration) (bool, net.Conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.players[playerID]
	if !ok {
		return false, nil // already removed by another path
	}
	now := time.Now()
	var stale bool
	if p.LastSeen.IsZero() {
		stale = now.Sub(s.CreatedAt) > timeout
	} else {
		stale = now.Sub(p.LastSeen) > timeout
	}
	if !stale {
		return false, nil // player became active between Phase 1 and Phase 2
	}
	tcpConn := p.TCPConn
	delete(s.players, playerID)
	return true, tcpConn
}

// ReapStaleSessions removes stale players and empty sessions.
// Returns counts of removed players and sessions for logging.
//
// Two-phase approach to minimise routing blackouts:
//   - Phase 1: RLock — scan for stale data without blocking the hot path
//     (FindSessionByPlayer also only needs RLock, so routing is unaffected).
//   - Phase 2: Lock — briefly apply the removals (fast map deletes only).
func (sm *SessionManager) ReapStaleSessions(logger *slog.Logger) (removedPlayers, removedSessions int) {
	type reapCandidate struct {
		sessionID string
		session   *Session
		stalePIDs []uint32
	}

	// Phase 1: collect stale data under read-lock (routing not blocked).
	sm.mu.RLock()
	now := time.Now()
	var candidates []reapCandidate
	for sid, s := range sm.sessions {
		stalePIDs := s.StalePlayers(PlayerTimeout)
		isEmpty := s.PlayerCount() == 0 && now.Sub(s.CreatedAt) > SessionEmptyTimeout
		if len(stalePIDs) > 0 || isEmpty {
			candidates = append(candidates, reapCandidate{sid, s, stalePIDs})
		}
	}
	sm.mu.RUnlock()

	if len(candidates) == 0 {
		return
	}

	// Phase 2: apply removals under write-lock (brief — only touches candidates).
	sm.mu.Lock()
	defer sm.mu.Unlock()
	now = time.Now()
	for _, c := range candidates {
		for _, pid := range c.stalePIDs {
			// Re-verify staleness; player may have become active since Phase 1.
			removed, tcpConn := c.session.RemovePlayerIfStale(pid, PlayerTimeout)
			if removed {
				if tcpConn != nil {
					go tcpConn.Close() // evict the goroutine asynchronously
				}
				delete(sm.playerIndex, pid)
				sm.metrics.ActivePlayers.Add(-1)
				removedPlayers++
				logger.Info("Reaped stale player", "session", c.sessionID, "player", pid)
			}
		}
		// Reap session if it is now empty and old enough.
		if c.session.PlayerCount() == 0 && now.Sub(c.session.CreatedAt) > SessionEmptyTimeout {
			delete(sm.sessions, c.sessionID)
			sm.metrics.ActiveSessions.Add(-1)
			removedSessions++
			logger.Info("Reaped empty session", "session", c.sessionID)
		}
	}
	return
}

// RunReaper periodically cleans up stale players and empty sessions.
// Also logs server stats every cycle. Blocks until context is cancelled.
func (sm *SessionManager) RunReaper(ctx context.Context, logger *slog.Logger) {
	ticker := time.NewTicker(ReaperInterval)
	defer ticker.Stop()

	lastWall := time.Now()
	lastCPU := processCPUTime()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			players, sessions := sm.ReapStaleSessions(logger)
			if players > 0 || sessions > 0 {
				logger.Info("Reaper cycle complete",
					"removed_players", players,
					"removed_sessions", sessions)
			}

			// Calculate CPU usage percentage since last cycle
			nowWall := time.Now()
			nowCPU := processCPUTime()
			wallDelta := nowWall.Sub(lastWall)
			cpuDelta := nowCPU - lastCPU
			lastWall = nowWall
			lastCPU = nowCPU

			var cpuPct float64
			if wallDelta > 0 {
				cpuPct = float64(cpuDelta) / float64(wallDelta) / float64(runtime.NumCPU()) * 100.0
			}

			// Always log current server stats
			snap := sm.metrics.Snapshot()
			logger.Info("server stats",
				"active_sessions", snap.ActiveSessions,
				"active_players", snap.ActivePlayers,
				"packets_routed", snap.PacketsRouted,
				"bytes_routed_mb", snap.BytesRouted/1024/1024,
				"packets_dropped", snap.PacketsDropped,
				"cpu_pct", fmt.Sprintf("%.1f", cpuPct),
			)
		}
	}
}

