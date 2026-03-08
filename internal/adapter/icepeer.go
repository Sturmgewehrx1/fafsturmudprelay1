package adapter

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/stun/v3"
	"github.com/pion/webrtc/v4"
)

// The java-ice-adapter multiplexes game data and keepalive echoes on the same
// ICE connection using a single-byte prefix:
//
//	0x64 ('d') — game data packet; the remaining bytes are forwarded to FA
//	0x65 ('e') — echo packet; 9 bytes total: 'e' + 8-byte big-endian timestamp
//
// The offering peer sends echoes every second; the answering peer bounces them
// back unchanged.  If the offerer receives no valid echo response for 10 s it
// declares the connection lost and restarts the ICE agent.
const (
	fafPrefixData        byte = 'd'
	fafPrefixEcho        byte = 'e'
	fafPrefixAnswerEcho  byte = 'f' // answerer→offerer echo for clock-independent RTT
	fafPrefixRTTReport   byte = 'r' // offerer→answerer: push measured RTT so answerer can display it
	fafEchoLen                = 9   // 'e'/'f' + 8-byte int64 timestamp
	fafRTTReportLen           = 5   // 'r' + 4-byte int32 RTT in ms
)

// gatherTimeout is the maximum time to wait for ICE candidate gathering.
const gatherTimeout = 5 * time.Second

// signalingTimeout is the time between credential re-sends when the remote
// has not yet responded.  Each timeout re-sends our credentials (in case the
// FAF signaling message was dropped) without creating a new peer or consuming
// a retry attempt.
const signalingTimeout = 30 * time.Second

// answererRestartDelay is how long an answerer (controlled side) waits before
// firing a self-initiated restart after ICE failure.  This gives the remote
// offerer (controlling side, typically the java-ice-adapter) time to initiate
// the restart from its side.  If the offerer's credentials arrive during this
// window (via HandleIceMsg), restartOnce suppresses the delayed self-restart.
const answererRestartDelay = 5 * time.Second

// fafIceMsg is the FAF java-ice-adapter ICE signaling message format.
// Contains ICE credentials (ufrag/password) and all gathered candidates.
type fafIceMsg struct {
	SrcID      int               `json:"srcId"`
	DestID     int               `json:"destId"`
	Password   string            `json:"password"`
	Ufrag      string            `json:"ufrag"`
	Candidates []fafIceCandidate `json:"candidates"`
	Manual     bool              `json:"-"` // internal: manual restart from debug UI, bypasses failure limit and backoff
}

// fafIceCandidate is a single ICE candidate in FAF format.
type fafIceCandidate struct {
	Foundation string  `json:"foundation"`
	Protocol   string  `json:"protocol"`
	Priority   uint32  `json:"priority"`
	IP         string  `json:"ip"`
	Port       int     `json:"port"`
	Type       string  `json:"type"` // HOST_CANDIDATE / SERVER_REFLEXIVE_CANDIDATE / RELAYED_CANDIDATE
	Generation int     `json:"generation"`
	ID         string  `json:"id"`
	RelAddr    *string `json:"relAddr"`
	RelPort    int     `json:"relPort"`
}

// ICEPeer represents a per-peer bidirectional forwarder using raw ICE (pion/ice).
// Used when the remote peer runs the original FAF java-ice-adapter instead of
// our relay adapter.  Data is transported as raw UDP bytes over the ICE
// connection — no DTLS, no SCTP, no DataChannel.
type ICEPeer struct {
	PlayerID      int
	Login         string
	Offerer       bool // true = controlling (we Dial); false = controlled (we Accept)
	localPlayerID int  // our player ID, used in outgoing FAF ICE messages

	conn     *net.UDPConn // local UDP socket the game engine talks to
	ownsConn bool         // true if we created conn and must close it on Close()
	gameAddr *net.UDPAddr // learned from first incoming game packet
	mu       sync.RWMutex

	agent     *ice.Agent
	iceConn   net.Conn    // set after ICE connection is established
	connReady chan struct{} // closed when iceConn is set

	remoteMsg   chan fafIceMsg // buffered channel for incoming FAF ICE messages
	remoteUfrag string        // last accepted remote ufrag — used for ICE restart detection

	// Gathering state — populated by OnCandidate callback, read by run()
	gathered   []ice.Candidate
	gatheredMu sync.Mutex
	gatherDone chan struct{} // closed when gathering completes (nil candidate)

	onIceMsg    func(msg string)    // callback: sends onIceMsg notification to FAF client
	onConnected func()             // callback: notify FAF client that ICE peer is connected
	onRestart   func(msg fafIceMsg) // callback: called on ICE restart (remote-triggered or self-initiated)
	logger      *slog.Logger
	ctx         context.Context    // set by Start(); used for delayed-restart cancellation
	cancel      context.CancelFunc
	started     bool
	relayOnly   bool // if true, only relay candidates are advertised in FAF ICE message
	sendFirst   bool // if true, answerer sends credentials before waiting for remote (self-initiated restart)
	wg          sync.WaitGroup // tracks run() and gameToICE() goroutines for clean shutdown

	// restartOnce ensures we only fire onRestart once per ICE session, even if both
	// Dial() error and the ConnectionStateFailed callback race to trigger a restart.
	restartOnce sync.Once

	AddedOrder   int       // insertion order assigned by PeerManager; stable across ICE restarts
	iceState     string   // current ICE connection state string, protected by mu
	connectedAt  time.Time // when ICE state first became Connected, protected by mu
	rttMs        int64    // echo/NTP-based RTT in ms (fallback only), protected by mu
	rttEstimated bool     // true if rttMs is an NTP-based 2× one-way estimate, protected by mu
}

// NewICEPeer creates a new ICE peer with a local UDP socket and a pion/ice Agent.
// The ICE agent immediately begins gathering candidates.  Call Start() to begin
// the ICE signaling and data forwarding goroutines.
//
// If existingConn is non-nil it is reused as the local UDP socket (same port).
// This is used on ICE restarts so the game does not need to reconnect to a new port.
func NewICEPeer(playerID int, login string, offerer bool, localPlayerID int, iceServers []webrtc.ICEServer, onIceMsg func(string), onConnected func(), onRestart func(fafIceMsg), logger *slog.Logger, existingConn *net.UDPConn) (*ICEPeer, error) {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}

	// Reuse the caller-supplied socket (ICE restart) or bind a fresh one.
	var conn *net.UDPConn
	if existingConn != nil {
		conn = existingConn
	} else {
		laddr, _ := net.ResolveUDPAddr("udp", "127.0.0.1:0")
		var err error
		conn, err = net.ListenUDP("udp", laddr)
		if err != nil {
			return nil, fmt.Errorf("bind local UDP: %w", err)
		}
	}

	// Convert webrtc.ICEServer → []*stun.URI for pion/ice
	var stunURIs []*stun.URI
	for _, s := range iceServers {
		uris, err := iceServerToStunURIs(s)
		if err != nil {
			logger.Warn("skipping ICE server", "urls", s.URLs, "error", err)
			continue
		}
		stunURIs = append(stunURIs, uris...)
	}

	// CheckInterval: send connectivity checks every 20 ms (default 200 ms).
	// With ~95 candidate pairs (19 local × 5 remote), the default 200 ms
	// means it takes 19 seconds to START checking all pairs.  Relay pairs
	// (lowest priority) begin around position 70-95, so at 200 ms/pair they
	// start at 14-19 seconds — well past the 5 s timeout.  At 20 ms all
	// pairs are tried within ~2 s.  Bandwidth cost: ~10 KB total burst.
	//
	// AcceptanceMinWait (all 0): pion normally delays nominating lower-quality
	// candidate types (srflx 500 ms, prflx 1 s, relay 2 s) to prefer faster
	// paths.  We set all to 0 because speed matters more than finding the
	// optimal path — if a relay pair is the first to succeed, use it now.
	// Higher-priority pairs that are also working will have already succeeded
	// (they are checked first) and would be nominated instead.
	//
	// MaxBindingRequests: 4 (default 7).  Failing pairs are abandoned sooner,
	// freeing resources.  4 retries is plenty even for 500 ms RTT connections.
	//
	// KeepaliveInterval: 500 ms.  Commercial VPNs (e.g. Proton VPN) have
	// aggressive UDP state timeouts; 500 ms keeps their NAT mapping alive.
	//
	// DisconnectedTimeout: 3 s.  After a keepalive goes unanswered the agent
	// transitions to Disconnected; 3 s gives transient packet loss time to
	// recover without waiting unnecessarily long.
	//
	// FailedTimeout: 2 s.  Once Disconnected we give another 2 s for the
	// path to recover.  Total timeout (3+2=5 s) is enough: with 20 ms
	// CheckInterval all pairs are tried by 2 s, so if nothing works by 5 s
	// it won't work at all.  Still comfortable for 500 ms RTT players.
	checkInterval := 20 * time.Millisecond
	zeroWait := time.Duration(0)
	maxBindingRequests := uint16(4)
	keepaliveInterval := 500 * time.Millisecond
	disconnectedTimeout := 3 * time.Second
	failedTimeout := 2 * time.Second
	agent, err := ice.NewAgent(&ice.AgentConfig{
		Urls:                   stunURIs,
		NetworkTypes:           []ice.NetworkType{ice.NetworkTypeUDP4},
		CheckInterval:          &checkInterval,
		HostAcceptanceMinWait:  &zeroWait,
		SrflxAcceptanceMinWait: &zeroWait,
		PrflxAcceptanceMinWait: &zeroWait,
		RelayAcceptanceMinWait: &zeroWait,
		MaxBindingRequests:     &maxBindingRequests,
		KeepaliveInterval:      &keepaliveInterval,
		DisconnectedTimeout:    &disconnectedTimeout,
		FailedTimeout:          &failedTimeout,
	})
	if err != nil {
		if existingConn == nil {
			conn.Close() // only close if we created it
		}
		return nil, fmt.Errorf("create ICE agent: %w", err)
	}

	p := &ICEPeer{
		PlayerID:      playerID,
		Login:         login,
		Offerer:       offerer,
		localPlayerID: localPlayerID,
		conn:          conn,
		ownsConn:      existingConn == nil,
		agent:         agent,
		connReady:     make(chan struct{}),
		remoteMsg:     make(chan fafIceMsg, 4),
		gatherDone:    make(chan struct{}),
		onIceMsg:      onIceMsg,
		onConnected:   onConnected,
		onRestart:     onRestart,
		logger:        logger,
	}

	// Collect gathered candidates; nil candidate signals gathering complete.
	agent.OnCandidate(func(c ice.Candidate) {
		if c == nil {
			close(p.gatherDone)
			return
		}
		p.gatheredMu.Lock()
		p.gathered = append(p.gathered, c)
		p.gatheredMu.Unlock()
	})

	agent.OnConnectionStateChange(func(state ice.ConnectionState) {
		now := time.Now()
		p.mu.Lock()
		p.iceState = state.String()
		if state == ice.ConnectionStateConnected {
			p.connectedAt = now
		} else {
			p.connectedAt = time.Time{} // reset on any non-Connected state
		}
		p.mu.Unlock()
		p.logger.Info("ICE connection state", "peer", playerID, "state", state)
		if state == ice.ConnectionStateConnected {
			p.logger.Info("reconnect button will be available at",
				"peer", playerID, "available_at", now.Add(120*time.Second).Format("15:04:05"))
			if p.onConnected != nil {
				p.onConnected()
			}
		}
		if state == ice.ConnectionStateFailed {
			p.logger.Warn("ICE connection failed", "peer", playerID)
			if p.onRestart == nil {
				return
			}
			if p.Offerer {
				// Offerer (controlling): restart immediately.
				p.restartOnce.Do(func() { go p.onRestart(fafIceMsg{}) })
			} else {
				// Answerer (controlled): wait briefly for the remote offerer
				// to initiate the restart.  The offerer is the ICE-controlling
				// side and should be the one to send new credentials first.
				// If a remote-triggered restart arrives during this delay
				// (via HandleIceMsg → restartOnce), the delayed self-restart
				// is suppressed.  If nothing arrives, we fall back to a
				// self-restart with sendFirst.
				go func() {
					select {
					case <-time.After(answererRestartDelay):
					case <-p.ctx.Done():
						return
					}
					p.restartOnce.Do(func() { go p.onRestart(fafIceMsg{}) })
				}()
			}
		}
	})

	// Begin gathering immediately (results arrive via OnCandidate callback).
	if err := agent.GatherCandidates(); err != nil {
		if existingConn == nil {
			conn.Close() // only close if we created it
		}
		agent.Close()
		return nil, fmt.Errorf("gather candidates: %w", err)
	}

	return p, nil
}

// HandleIceMsg processes an incoming FAF ICE message (ufrag + password + candidates).
// If the message carries different credentials than the currently active session,
// it is treated as an ICE restart: the onRestart callback is invoked so the caller
// can tear down this peer and create a fresh one.
func (p *ICEPeer) HandleIceMsg(msgJSON string) error {
	p.logger.Info("iceMsg received", "peer", p.PlayerID, "raw", msgJSON)

	var msg fafIceMsg
	if err := json.Unmarshal([]byte(msgJSON), &msg); err != nil {
		return fmt.Errorf("parse faf iceMsg: %w", err)
	}
	if msg.Ufrag == "" || msg.Password == "" {
		p.logger.Warn("iceMsg missing credentials, ignoring", "peer", p.PlayerID)
		return nil
	}

	// Detect ICE restart: the remote peer has sent new credentials.  The
	// java-ice-adapter creates a brand-new ICE agent (with new ufrag/password)
	// when it loses connectivity — this can happen while we are still in the
	// Checking state (Accept/Dial running), not only after a successful connect.
	// We must detect restarts in both cases; pion/ice does not support in-place
	// restarts so we always create a fresh agent.
	p.mu.RLock()
	currentUfrag := p.remoteUfrag
	p.mu.RUnlock()

	if currentUfrag != "" && msg.Ufrag != currentUfrag {
		p.logger.Info("ICE restart detected (new credentials from remote)",
			"peer", p.PlayerID, "old_ufrag", currentUfrag, "new_ufrag", msg.Ufrag)
		if p.onRestart != nil {
			// Fire the restart callback directly — NOT through restartOnce.
			// Remote restarts carry credentials from the offerer (including
			// host candidates that responses to sendFirst often lack) and must
			// always be processed, even when a self-initiated restart is already
			// sleeping in its backoff.  The restart callback uses a peer-identity
			// check after the backoff to detect that a concurrent remote restart
			// already replaced the peer, and aborts the stale self-restart.
			go p.onRestart(msg)
		}
		return nil
	}

	// Normal flow: push to channel — prefer newest credentials on overflow.
	select {
	case p.remoteMsg <- msg:
	default:
		select {
		case <-p.remoteMsg:
		default:
		}
		p.remoteMsg <- msg
	}
	return nil
}

// CreateOffer is a no-op in the raw ICE implementation.
// The FAF ICE message (with our credentials + candidates) is sent
// automatically by run() after gathering completes.
func (p *ICEPeer) CreateOffer() error {
	return nil
}

// Start begins the ICE signaling and data forwarding goroutines.
func (p *ICEPeer) Start(ctx context.Context) {
	p.mu.Lock()
	if p.started {
		p.mu.Unlock()
		return
	}
	p.started = true
	ctx, p.cancel = context.WithCancel(ctx)
	p.ctx = ctx
	p.mu.Unlock()

	p.wg.Add(2)
	go func() {
		defer p.wg.Done()
		p.run(ctx)
	}()
	go func() {
		defer p.wg.Done()
		p.gameToICE(ctx)
	}()
}

// SetRelayOnly forces this peer to advertise only relay (TURN) candidates in
// its FAF ICE message.  Must be called before Start().  Use this on ICE restart
// when the previous connection dropped shortly after being established, which
// indicates the remote NAT mapping expired (e.g. Russian ISP ~15 s UDP timeout).
// Without relay-only mode pion/ice would again prefer the higher-priority srflx
// pair, re-establish the same NAT mapping, and drop again after ~15 s.
func (p *ICEPeer) SetRelayOnly() {
	p.relayOnly = true
}

// SetSendFirst makes an answerer peer send its credentials before waiting for
// the remote.  Must be called before Start().  Use this on self-initiated ICE
// restarts from the answerer side: the remote java-ice-adapter does not know
// we restarted, so we must send our new ufrag first to trigger it to create a
// fresh agent and respond with new credentials.
func (p *ICEPeer) SetSendFirst() {
	p.sendFirst = true
}

// run is the main ICE lifecycle goroutine:
//  1. Wait for candidate gathering (with timeout)
//  2. For offerer (or answerer with sendFirst): send our FAF ICE message, then wait for remote
//  3. For answerer: wait for remote FAF ICE message, then send ours
//  4. Apply remote candidates and connect (Dial or Accept)
//  5. Start ICE→game forwarding
func (p *ICEPeer) run(ctx context.Context) {
	// 1. Wait for gathering to complete (or timeout).
	select {
	case <-p.gatherDone:
		p.logger.Info("ICE gathering complete", "peer", p.PlayerID)
	case <-time.After(gatherTimeout):
		p.logger.Warn("ICE gathering timeout, proceeding with available candidates", "peer", p.PlayerID)
	case <-ctx.Done():
		return
	}

	// Snapshot gathered candidates and local credentials.
	p.gatheredMu.Lock()
	localCandidates := make([]ice.Candidate, len(p.gathered))
	copy(localCandidates, p.gathered)
	p.gatheredMu.Unlock()

	localUfrag, localPwd, err := p.agent.GetLocalUserCredentials()
	if err != nil {
		p.logger.Error("failed to get local ICE credentials", "peer", p.PlayerID, "error", err)
		return
	}
	p.logger.Info("local ICE credentials", "peer", p.PlayerID, "ufrag", localUfrag, "candidates", len(localCandidates))

	// Log candidate type summary with addresses for diagnostics.
	candTypeCounts := map[string]int{}
	for _, c := range localCandidates {
		typ := c.Type().String()
		candTypeCounts[typ]++
		addr := c.Address()
		if relay, ok := c.(*ice.CandidateRelay); ok {
			p.logger.Info("local candidate", "peer", p.PlayerID,
				"type", typ, "addr", addr, "relayProto", relay.RelayProtocol())
		} else {
			p.logger.Info("local candidate", "peer", p.PlayerID,
				"type", typ, "addr", addr)
		}
	}
	p.logger.Info("local candidate summary", "peer", p.PlayerID,
		"host", candTypeCounts["host"], "srflx", candTypeCounts["srflx"],
		"relay", candTypeCounts["relay"], "prflx", candTypeCounts["prflx"])

	// 2. For offerer: send our message first, then wait for remote.
	//    For answerer with sendFirst (self-initiated restart): also send first
	//    so the remote java-ice-adapter detects the new ufrag and responds.
	if p.Offerer || p.sendFirst {
		p.sendFafIceMsg(localUfrag, localPwd, localCandidates)
	}

	// 3. Wait for remote FAF ICE message.
	//    On timeout, re-send our credentials in case the FAF signaling message
	//    was dropped.  We do NOT trigger a restart or create a new peer — that
	//    would waste a retry attempt and cause a race condition with the remote
	//    offerer's restart.  The peer stays alive and keeps listening.
	var remoteMsg fafIceMsg
	for {
		select {
		case remoteMsg = <-p.remoteMsg:
			p.logger.Info("processing remote ICE message", "peer", p.PlayerID,
				"ufrag", remoteMsg.Ufrag, "candidates", len(remoteMsg.Candidates))
			goto gotRemoteMsg
		case <-time.After(signalingTimeout):
			if p.Offerer || p.sendFirst {
				p.logger.Warn("ICE signaling timeout: re-sending credentials",
					"peer", p.PlayerID, "timeout", signalingTimeout)
				p.sendFafIceMsg(localUfrag, localPwd, localCandidates)
			} else {
				p.logger.Warn("ICE signaling timeout: still waiting for remote credentials",
					"peer", p.PlayerID, "timeout", signalingTimeout)
			}
		case <-ctx.Done():
			return
		}
	}
gotRemoteMsg:

	// Record remote ufrag so HandleIceMsg can detect future ICE restarts.
	p.mu.Lock()
	p.remoteUfrag = remoteMsg.Ufrag
	p.mu.Unlock()

	// For answerer (normal flow): send our message after receiving theirs.
	// Skip if sendFirst already sent above to avoid a duplicate message.
	if !p.Offerer && !p.sendFirst {
		p.sendFafIceMsg(localUfrag, localPwd, localCandidates)
	}

	// 4. Add remote candidates to the ICE agent.
	remoteCandTypeCounts := map[string]int{}
	for _, fc := range remoteMsg.Candidates {
		cand, err := fafCandToICE(fc)
		if err != nil {
			p.logger.Warn("skipping invalid remote candidate", "peer", p.PlayerID, "error", err)
			continue
		}
		remoteCandTypeCounts[cand.Type().String()]++
		p.logger.Info("remote candidate", "peer", p.PlayerID,
			"type", cand.Type().String(), "addr", cand.Address())
		if err := p.agent.AddRemoteCandidate(cand); err != nil {
			p.logger.Warn("failed to add remote candidate", "peer", p.PlayerID, "error", err)
		}
	}
	p.logger.Info("remote candidate summary", "peer", p.PlayerID,
		"host", remoteCandTypeCounts["host"], "srflx", remoteCandTypeCounts["srflx"],
		"relay", remoteCandTypeCounts["relay"], "prflx", remoteCandTypeCounts["prflx"])

	// 5. Connect (blocking).  Dial = controlling, Accept = controlled.
	var iceConn net.Conn
	if p.Offerer {
		iceConn, err = p.agent.Dial(ctx, remoteMsg.Ufrag, remoteMsg.Password)
	} else {
		iceConn, err = p.agent.Accept(ctx, remoteMsg.Ufrag, remoteMsg.Password)
	}
	if err != nil {
		p.logger.Error("ICE connect failed", "peer", p.PlayerID, "error", err)
		// OnConnectionStateChange(Failed) may fire concurrently; restartOnce ensures
		// only one restart is triggered regardless of which path fires first.
		if p.Offerer && p.onRestart != nil {
			p.restartOnce.Do(func() { go p.onRestart(fafIceMsg{}) })
		}
		return
	}

	p.mu.Lock()
	p.iceConn = iceConn
	p.mu.Unlock()
	close(p.connReady)
	p.logger.Info("ICE connected", "peer", p.PlayerID)

	// Log selected candidate pair for diagnostics.
	if pair, pairErr := p.agent.GetSelectedCandidatePair(); pairErr == nil && pair != nil {
		localAddr := pair.Local.Address() + ":" + strconv.Itoa(pair.Local.Port())
		remoteAddr := pair.Remote.Address() + ":" + strconv.Itoa(pair.Remote.Port())
		logArgs := []any{
			"peer", p.PlayerID,
			"localType", pair.Local.Type().String(),
			"localAddr", localAddr,
			"remoteType", pair.Remote.Type().String(),
			"remoteAddr", remoteAddr,
		}
		if relay, ok := pair.Local.(*ice.CandidateRelay); ok {
			logArgs = append(logArgs, "relayProto", relay.RelayProtocol())
		}
		if relay, ok := pair.Remote.(*ice.CandidateRelay); ok {
			logArgs = append(logArgs, "remoteRelayProto", relay.RelayProtocol())
		}
		p.logger.Info("ICE selected pair", logArgs...)
	}

	// 6. Start echo loops.
	//    Offerer sends 'e' echoes (required by java-ice-adapter connectivity checker).
	//    Answerer sends 'f' echoes so the offerer reflects them back for direct RTT
	//    measurement without relying on NTP clock sync.
	if p.Offerer {
		go p.sendEchoLoop(ctx, iceConn)
	} else {
		go p.sendAnswerEchoLoop(ctx, iceConn)
	}

	// 7. Start ICE→game forwarding (game→ICE is already running from Start).
	p.iceToGame(ctx, iceConn)
}

// sendFafIceMsg sends our ICE credentials and candidates to the FAF client
// in the java-ice-adapter JSON format.
// When relayOnly is set, non-relay (host/srflx) candidates are omitted so that
// pion/ice on the remote side is forced to select a relay pair instead of the
// higher-priority srflx pair that would otherwise be chosen.
func (p *ICEPeer) sendFafIceMsg(ufrag, pwd string, candidates []ice.Candidate) {
	fafCandidates := make([]fafIceCandidate, 0, len(candidates))
	for i, c := range candidates {
		if p.relayOnly && c.Type() != ice.CandidateTypeRelay {
			continue
		}
		fafCandidates = append(fafCandidates, iceToFAFCand(c, i))
	}
	if p.relayOnly {
		p.logger.Info("relay-only mode: advertising only relay candidates",
			"peer", p.PlayerID, "count", len(fafCandidates))
	}

	msg := fafIceMsg{
		SrcID:      p.localPlayerID,
		DestID:     p.PlayerID,
		Password:   pwd,
		Ufrag:      ufrag,
		Candidates: fafCandidates,
	}
	msgJSON, _ := json.Marshal(msg)
	p.onIceMsg(string(msgJSON))
	p.logger.Info("sent FAF ICE message", "peer", p.PlayerID, "candidates", len(fafCandidates))
}

// iceToGame reads packets from the ICE connection, strips the FAF prefix byte,
// and forwards game data to the local UDP socket.
//
// Protocol (java-ice-adapter):
//
//	0x64 ('d') + payload  → game data; forward payload to ForgedAlliance
//	0x65 ('e') + 8 bytes  → echo packet
//	  • answerer: bounce the 9-byte packet back unchanged
//	  • offerer:  handled by sendEchoLoop / ignored here (RTT tracking omitted)
func (p *ICEPeer) iceToGame(ctx context.Context, conn net.Conn) {
	buf := make([]byte, 65537) // +1 for the prefix byte

	// Early data buffer: game data ('d') packets that arrive before the game
	// has sent its first UDP packet to our local socket (gameAddr still nil).
	// This fixes a race condition where ICE connects before the game processes
	// the JoinGame/ConnectToPeer command.  Without buffering, the initial
	// handshake packets from the remote peer are silently dropped and the game
	// stays on "Connecting…" indefinitely.
	const earlyBufCap = 64
	var earlyBuf [][]byte

	// Log the first game data packet received via ICE so we can diagnose
	// "Connected but game shows Connecting…" issues from the logs alone.
	firstDataReceived := false

	// Per-prefix deduplication for unknown-prefix warnings.
	// Some older java-ice-adapter versions send game packets without the 'd'
	// prefix (e.g. 0xC3 = 195).  The first occurrence is logged as WARN;
	// subsequent occurrences are dropped silently to avoid log spam.
	unknownPrefixWarned := make(map[byte]bool)

	for {
		n, err := conn.Read(buf)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			p.logger.Error("ICE read error", "peer", p.PlayerID, "error", err)
			return
		}
		if n == 0 {
			continue
		}

		switch buf[0] {
		case fafPrefixData:
			// Strip 'd' prefix, forward remainder to game.
			payload := buf[1:n]
			if len(payload) == 0 {
				break
			}
			if !firstDataReceived {
				firstDataReceived = true
				p.logger.Info("first ICE→game data received", "peer", p.PlayerID, "len", len(payload))
			}
			p.mu.RLock()
			addr := p.gameAddr
			p.mu.RUnlock()
			if addr == nil {
				// Game hasn't sent its first packet yet — buffer for later replay.
				if len(earlyBuf) < earlyBufCap {
					pkt := make([]byte, len(payload))
					copy(pkt, payload)
					earlyBuf = append(earlyBuf, pkt)
				} else {
					p.logger.Warn("dropping ICE→game data, game address unknown and buffer full",
						"peer", p.PlayerID, "buffered", len(earlyBuf))
				}
				break
			}
			// Replay any buffered packets before forwarding the current one.
			if len(earlyBuf) > 0 {
				p.logger.Info("replaying early-buffered ICE→game packets",
					"peer", p.PlayerID, "count", len(earlyBuf))
				for _, pkt := range earlyBuf {
					if _, err := p.conn.WriteToUDP(pkt, addr); err != nil {
						p.logger.Error("ICE peer local write error (buffered)", "peer", p.PlayerID, "error", err)
					}
				}
				earlyBuf = nil
			}
			if _, err := p.conn.WriteToUDP(payload, addr); err != nil {
				p.logger.Error("ICE peer local write error", "peer", p.PlayerID, "error", err)
			}

		case fafPrefixEcho:
			if n == fafEchoLen {
				if p.Offerer {
					// Offerer: measure RTT from the timestamp echoed back by the answerer.
					ts := int64(buf[1])<<56 | int64(buf[2])<<48 | int64(buf[3])<<40 |
						int64(buf[4])<<32 | int64(buf[5])<<24 | int64(buf[6])<<16 |
						int64(buf[7])<<8 | int64(buf[8])
					rtt := time.Now().UnixMilli() - ts
					if rtt >= 0 && rtt < 60000 { // sanity check
						p.mu.Lock()
						p.rttMs = rtt
						p.mu.Unlock()
						// Push RTT to the answerer so it can display it without NTP or 'f' echo.
						// Old answerers drop unknown prefix bytes — no harm done.
						var rttPkt [fafRTTReportLen]byte
						rttPkt[0] = fafPrefixRTTReport
						rttPkt[1] = byte(rtt >> 24)
						rttPkt[2] = byte(rtt >> 16)
						rttPkt[3] = byte(rtt >> 8)
						rttPkt[4] = byte(rtt)
						conn.Write(rttPkt[:]) //nolint:errcheck
					}
				} else {
					// Answerer: NTP-based 2× one-way estimate (fallback if remote does not
					// support 'f' echo reflection, e.g. java-ice-adapter as offerer).
					ts := int64(buf[1])<<56 | int64(buf[2])<<48 | int64(buf[3])<<40 |
						int64(buf[4])<<32 | int64(buf[5])<<24 | int64(buf[6])<<16 |
						int64(buf[7])<<8 | int64(buf[8])
					oneWay := time.Now().UnixMilli() - ts
					if oneWay >= 0 && oneWay < 30000 {
						p.mu.Lock()
						// Only fall back to NTP estimate if no direct measurement exists yet.
						if p.rttMs == 0 {
							p.rttMs = oneWay * 2
							p.rttEstimated = true
						}
						p.mu.Unlock()
					}
					// Mirror echo back unchanged so the offerer's connectivity checker resets.
					conn.Write(buf[:n]) //nolint:errcheck
				}
			}

		case fafPrefixAnswerEcho:
			if n == fafEchoLen {
				if p.Offerer {
					// Offerer: reflect the answerer's 'f' echo back so the answerer can
					// measure RTT directly without relying on clock synchronisation.
					conn.Write(buf[:n]) //nolint:errcheck
				} else {
					// Answerer: our own 'f' echo was reflected back — measure RTT directly.
					ts := int64(buf[1])<<56 | int64(buf[2])<<48 | int64(buf[3])<<40 |
						int64(buf[4])<<32 | int64(buf[5])<<24 | int64(buf[6])<<16 |
						int64(buf[7])<<8 | int64(buf[8])
					rtt := time.Now().UnixMilli() - ts
					if rtt >= 0 && rtt < 60000 {
						p.mu.Lock()
						p.rttMs = rtt
						p.rttEstimated = false
						p.mu.Unlock()
					}
				}
			}

		case fafPrefixRTTReport:
			// Answerer receives RTT measured by the offerer — no NTP or 'f'-echo needed.
			// Offerer sends this immediately after each echo roundtrip measurement.
			// Old answerers drop this unknown prefix byte silently.
			if n == fafRTTReportLen && !p.Offerer {
				rtt := int64(buf[1])<<24 | int64(buf[2])<<16 | int64(buf[3])<<8 | int64(buf[4])
				if rtt >= 0 && rtt < 60000 {
					p.mu.Lock()
					p.rttMs = rtt
					p.rttEstimated = false
					p.mu.Unlock()
				}
			}

		default:
			// STUN binding packets (first byte 0x00–0x03) can leak through pion/ice
			// demux during connection setup — drop silently without spamming the log.
			if buf[0] <= 0x03 {
				break
			}
			// Log only the first occurrence per prefix byte to avoid log spam.
			// Older java-ice-adapter versions can emit hundreds of these per second.
			if !unknownPrefixWarned[buf[0]] {
				unknownPrefixWarned[buf[0]] = true
				p.logger.Warn("ICE: unknown prefix byte, dropping packet (further occurrences suppressed)",
					"peer", p.PlayerID, "prefix", buf[0], "len", n)
			}
		}
	}
}

// gameToICE reads packets from the game engine via the local UDP socket,
// prepends the 'd' data prefix, and sends them through the ICE connection.
func (p *ICEPeer) gameToICE(ctx context.Context) {
	// Pre-allocate a send buffer with room for the 'd' prefix.
	buf := make([]byte, 65536)
	send := make([]byte, 65537)
	send[0] = fafPrefixData

	// Log the first game data packet forwarded via ICE so we can diagnose
	// "Connected but game shows Connecting…" issues from the logs alone.
	firstDataForwarded := false

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
			p.logger.Error("ICE peer local read error", "peer", p.PlayerID, "error", err)
			time.Sleep(udpErrorBackoff)
			continue
		}

		// Learn or update the game's address.  The initial address is learned
		// from the first packet.  If the game reconnects to our port (e.g. after
		// processing a second ConnectToPeer command), it may send from a different
		// source port — we must follow the new address so replies reach the right
		// socket.
		p.mu.Lock()
		if p.gameAddr == nil {
			p.gameAddr = remoteAddr
			p.logger.Info("learned game address for ICE peer", "peer", p.PlayerID, "addr", remoteAddr)
		} else if p.gameAddr.Port != remoteAddr.Port {
			p.logger.Info("game address changed for ICE peer", "peer", p.PlayerID,
				"old", p.gameAddr, "new", remoteAddr)
			p.gameAddr = remoteAddr
		}
		p.mu.Unlock()

		// Wait for ICE connection to be ready before sending.
		select {
		case <-p.connReady:
		case <-ctx.Done():
			return
		}

		p.mu.RLock()
		conn := p.iceConn
		p.mu.RUnlock()
		if conn != nil {
			copy(send[1:], buf[:n])
			if _, err := conn.Write(send[:n+1]); err != nil {
				p.logger.Error("ICE write error", "peer", p.PlayerID, "error", err)
			} else if !firstDataForwarded {
				firstDataForwarded = true
				p.logger.Info("first game→ICE data forwarded", "peer", p.PlayerID, "len", n)
			}
		}
	}
}

// sendEchoLoop sends 'e' echo packets every second so the remote
// java-ice-adapter connectivity checker resets its 10-second timeout.
// Only the offering (controlling) peer sends 'e' echoes.
func (p *ICEPeer) sendEchoLoop(ctx context.Context, conn net.Conn) {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	pkt := make([]byte, fafEchoLen)
	pkt[0] = fafPrefixEcho
	for {
		select {
		case <-ticker.C:
			now := time.Now().UnixMilli()
			pkt[1] = byte(now >> 56)
			pkt[2] = byte(now >> 48)
			pkt[3] = byte(now >> 40)
			pkt[4] = byte(now >> 32)
			pkt[5] = byte(now >> 24)
			pkt[6] = byte(now >> 16)
			pkt[7] = byte(now >> 8)
			pkt[8] = byte(now)
			conn.Write(pkt) //nolint:errcheck
		case <-ctx.Done():
			return
		}
	}
}

// sendAnswerEchoLoop sends 'f' echo packets every second so the offerer can
// reflect them back.  This gives the answerer a direct clock-independent RTT
// measurement.  If the remote offerer is the java-ice-adapter it will drop 'f'
// packets (unknown prefix), in which case the answerer falls back to the
// NTP-based 2× one-way estimate derived from received 'e' echo timestamps.
func (p *ICEPeer) sendAnswerEchoLoop(ctx context.Context, conn net.Conn) {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	pkt := make([]byte, fafEchoLen)
	pkt[0] = fafPrefixAnswerEcho
	for {
		select {
		case <-ticker.C:
			now := time.Now().UnixMilli()
			pkt[1] = byte(now >> 56)
			pkt[2] = byte(now >> 48)
			pkt[3] = byte(now >> 40)
			pkt[4] = byte(now >> 32)
			pkt[5] = byte(now >> 24)
			pkt[6] = byte(now >> 16)
			pkt[7] = byte(now >> 8)
			pkt[8] = byte(now)
			conn.Write(pkt) //nolint:errcheck
		case <-ctx.Done():
			return
		}
	}
}

// LocalPort returns the local UDP port the game should connect to for this peer.
func (p *ICEPeer) LocalPort() int {
	return p.conn.LocalAddr().(*net.UDPAddr).Port
}

// ICEState returns the current ICE connection state as a string (thread-safe).
func (p *ICEPeer) ICEState() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.iceState == "" {
		return "New"
	}
	return p.iceState
}

// RttMs returns the round-trip time in milliseconds (thread-safe).
// Prefers pion/ice STUN keepalive RTT (accurate for both Offerer and Answerer,
// no NTP required).  Falls back to echo-based / NTP value if not yet available.
func (p *ICEPeer) RttMs() int64 {
	// Primary: pion/ice measures RTT from STUN binding request/response for both sides.
	// NOTE: GetSelectedCandidatePair() returns a copy without RTT fields — must use Stats.
	// Guard: calling Stats on a closed agent logs a noisy error — skip if closed.
	p.mu.RLock()
	agentClosed := p.iceState == "Closed"
	rtt := p.rttMs
	p.mu.RUnlock()
	if !agentClosed {
		if stats, ok := p.agent.GetSelectedCandidatePairStats(); ok {
			if ms := int64(stats.CurrentRoundTripTime * 1000); ms > 0 {
				return ms
			}
		}
	}
	// Fallback: echo-based or NTP estimate.
	return rtt
}

// RttIsEstimated returns true if the current RTT is an NTP-based 2× one-way estimate
// rather than a direct measurement.  Always false when pion/ice STUN RTT is available.
func (p *ICEPeer) RttIsEstimated() bool {
	// STUN RTT is always a direct measurement — never estimated.
	// NOTE: GetSelectedCandidatePair() returns a copy without RTT fields — must use Stats.
	p.mu.RLock()
	agentClosed := p.iceState == "Closed"
	est := p.rttEstimated
	p.mu.RUnlock()
	if !agentClosed {
		if stats, ok := p.agent.GetSelectedCandidatePairStats(); ok && stats.CurrentRoundTripTime > 0 {
			return false
		}
	}
	return est
}

// SelectedTransport returns the transport protocol of the selected ICE candidate
// pair: "udp" for most connections, "tcp" when the selected local candidate is
// a TURN relay using TCP or TLS (TURNS) to the TURN server.
// Returns "udp" if no pair is selected yet or the local candidate is not a relay.
func (p *ICEPeer) SelectedTransport() string {
	p.mu.RLock()
	agentClosed := p.iceState == "Closed"
	p.mu.RUnlock()
	if agentClosed {
		return "udp"
	}
	pair, err := p.agent.GetSelectedCandidatePair()
	if err != nil || pair == nil {
		return "udp"
	}
	if relay, ok := pair.Local.(*ice.CandidateRelay); ok {
		proto := relay.RelayProtocol()
		if proto == "tcp" || proto == "tls" {
			return "tcp"
		}
	}
	return "udp"
}

// SelectedCandTypes returns the local and remote candidate types of the selected
// ICE candidate pair (e.g. "host", "srflx", "relay").
// Returns ("", "") if no pair has been selected yet.
func (p *ICEPeer) SelectedCandTypes() (localType, remoteType string) {
	p.mu.RLock()
	agentClosed := p.iceState == "Closed"
	p.mu.RUnlock()
	if agentClosed {
		return "", ""
	}
	pair, err := p.agent.GetSelectedCandidatePair()
	if err != nil || pair == nil {
		return "", ""
	}
	return pair.Local.Type().String(), pair.Remote.Type().String()
}

// Close stops the ICE peer and releases all resources.
//
// When the peer does NOT own the UDP socket (ownsConn == false, i.e. the socket
// will be transferred to a replacement peer on ICE restart), Close sets a past
// read deadline to unblock the gameToICE goroutine's ReadFromUDP call, waits for
// all goroutines to exit, then resets the deadline so the replacement peer can
// read normally.  This eliminates the race condition where two gameToICE
// goroutines read from the same socket concurrently, causing packet loss.
// ConnectedAt returns the time when ICE entered the Connected state.
// Returns zero time if not connected.
func (p *ICEPeer) ConnectedAt() time.Time {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.connectedAt
}

// ForceRestart triggers a manual ICE restart from the debug UI.
// Bypasses restartOnce (so it works even after an automatic restart)
// and sets the Manual flag so the restart callback skips failure limits
// and backoff delays.
func (p *ICEPeer) ForceRestart() {
	p.mu.RLock()
	connAt := p.connectedAt
	p.mu.RUnlock()
	if !connAt.IsZero() {
		p.logger.Info("reconnect triggered after connection duration",
			"peer", p.PlayerID, "connected_for", time.Since(connAt).Round(time.Second))
	}
	if p.onRestart != nil {
		go p.onRestart(fafIceMsg{Manual: true})
	}
}

func (p *ICEPeer) Close() {
	p.mu.Lock()
	cancel := p.cancel
	conn := p.iceConn
	p.iceConn = nil
	p.mu.Unlock()

	if cancel != nil {
		cancel()
	}

	// Unblock any pending ReadFromUDP in gameToICE by setting a past deadline.
	// Without this, gameToICE hangs in a blocking syscall that does not respond
	// to context cancellation, and wg.Wait() below would deadlock.
	if p.conn != nil {
		p.conn.SetReadDeadline(time.Now())
	}

	if conn != nil {
		conn.Close()
	}
	if p.agent != nil {
		p.agent.Close()
	}

	// Wait for run() and gameToICE() goroutines to finish before returning.
	// This guarantees that no old goroutine is still reading from the shared
	// UDP socket when the replacement peer starts.
	if p.started {
		p.wg.Wait()
	}

	if p.ownsConn {
		// Normal shutdown: close the socket now that goroutines have exited.
		p.conn.Close()
	} else if p.conn != nil {
		// ICE restart: reset the deadline so the replacement peer can read normally.
		p.conn.SetReadDeadline(time.Time{})
	}
}

// --- Candidate conversion helpers ---

// fafCandToICE converts a FAF-format candidate to a pion ICE candidate.
func fafCandToICE(c fafIceCandidate) (ice.Candidate, error) {
	typ := "host"
	switch c.Type {
	case "HOST_CANDIDATE":
		typ = "host"
	case "SERVER_REFLEXIVE_CANDIDATE":
		typ = "srflx"
	case "PEER_REFLEXIVE_CANDIDATE":
		typ = "prflx"
	case "RELAYED_CANDIDATE":
		typ = "relay"
	default:
		return nil, fmt.Errorf("unknown candidate type: %s", c.Type)
	}

	s := fmt.Sprintf("%s 1 %s %d %s %d typ %s",
		c.Foundation, strings.ToLower(c.Protocol), c.Priority, c.IP, c.Port, typ)
	if c.RelAddr != nil && *c.RelAddr != "" {
		s += fmt.Sprintf(" raddr %s rport %d", *c.RelAddr, c.RelPort)
	}
	return ice.UnmarshalCandidate(s)
}

// iceToFAFCand converts a pion ICE candidate to FAF format.
func iceToFAFCand(c ice.Candidate, idx int) fafIceCandidate {
	typ := "HOST_CANDIDATE"
	switch c.Type() {
	case ice.CandidateTypeServerReflexive:
		typ = "SERVER_REFLEXIVE_CANDIDATE"
	case ice.CandidateTypePeerReflexive:
		typ = "PEER_REFLEXIVE_CANDIDATE"
	case ice.CandidateTypeRelay:
		typ = "RELAYED_CANDIDATE"
	}

	fc := fafIceCandidate{
		Foundation: c.Foundation(),
		Protocol:   "udp",
		Priority:   c.Priority(),
		IP:         c.Address(),
		Port:       c.Port(),
		Type:       typ,
		ID:         strconv.Itoa(idx),
	}
	if rel := c.RelatedAddress(); rel != nil {
		addr := rel.Address
		fc.RelAddr = &addr
		fc.RelPort = rel.Port
	}
	return fc
}

// --- ICE server conversion ---

// iceServerToStunURIs converts a webrtc.ICEServer to pion/stun URIs.
func iceServerToStunURIs(server webrtc.ICEServer) ([]*stun.URI, error) {
	var uris []*stun.URI
	for _, rawURL := range server.URLs {
		uri, err := stun.ParseURI(rawURL)
		if err != nil {
			return nil, fmt.Errorf("parse ICE URL %q: %w", rawURL, err)
		}
		if server.Username != "" {
			uri.Username = server.Username
		}
		if cred, ok := server.Credential.(string); ok && cred != "" {
			uri.Password = cred
		}
		uris = append(uris, uri)
	}
	return uris, nil
}
