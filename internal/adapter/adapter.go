package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"
)

// Config holds adapter configuration (from CLI args).
type Config struct {
	PlayerID        int
	GameID          int
	Login           string
	RPCPort         int    // default 7236
	GPGNetPort      int    // 0 = auto
	LobbyPort       int    // 0 = auto
	RelayServer     string // e.g. "relay.example.com:10000" (UDP)
	RelayTCPServer  string // e.g. "relay.example.com:10001" (TCP fallback)
	RelayHTTP       string // e.g. "http://relay.example.com:8080"
	APIKey          string // if set, sent as Authorization: Bearer <key> to relay HTTP API
	TCPFallback     bool   // if true, fall back to TCP when UDP probe fails
	ForceTCP        bool   // if true, always use TCP (skip UDP probe entirely)
	DebugWindow     bool   // if true, start a local HTTP debug page and open it in the browser
	UseOfficialFAF  bool   // if true (host only), skip relay and use official FAF ICE connections
}

// Adapter is the main orchestrator that wires all components together.
type Adapter struct {
	cfg           Config
	ctx           context.Context
	cancel        context.CancelFunc
	playerID      int
	login         string
	rpcPort       int
	lobbyPort     int
	lobbyInitMode int
	isHost    bool // true after hostGame is called (this player is the lobby host)
	relayMode bool // true = use relay for all peers in this lobby (set in hostGame/joinGame)
	mu            sync.RWMutex // protects lobbyInitMode, isHost, relayMode, and iceServers
	iceServers    []webrtc.ICEServer
	logger        *slog.Logger

	peerMgr        *PeerManager
	relayConn      *RelayConnection
	gpgnet         *GPGNetServer
	rpc            *RPCServer
	relaySetupOnce sync.Once // guards ensureRelayReady (registration + hello)

	// gameDisconnectedCh is created by Run() so that the adapter shuts
	// down when ForgedAlliance.exe disconnects from the GPGNet server.
	// It is nil in unit tests that construct Adapter directly without Run().
	gameDisconnectedCh chan struct{}
}

// NewAdapter creates a new adapter instance.
// Always writes a log file next to the executable so logs are available
// even when the FAF client does not redirect stderr to a file.
func NewAdapter(cfg Config) *Adapter {
	var logWriter io.Writer = os.Stderr

	// Create log file next to the executable (same directory).
	if exePath, err := os.Executable(); err == nil {
		logDir := filepath.Dir(exePath)
		logName := filepath.Join(logDir, fmt.Sprintf("sturm-relay-adapter_%s.log",
			time.Now().Format("2006-01-02_15-04-05")))
		if f, err := os.Create(logName); err == nil {
			logWriter = io.MultiWriter(os.Stderr, f)
			fmt.Fprintf(os.Stderr, "Logging to %s\n", logName)
		}
	}

	logger := slog.New(slog.NewTextHandler(logWriter, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	return &Adapter{
		cfg:      cfg,
		playerID: cfg.PlayerID,
		login:    cfg.Login,
		rpcPort:  cfg.RPCPort,
		logger:   logger,
	}
}

// PlayerID returns the local player ID.
func (a *Adapter) PlayerID() int { return a.playerID }

// Login returns the local player login.
func (a *Adapter) Login() string { return a.login }

// LobbyPort returns the lobby UDP port.
func (a *Adapter) LobbyPort() int { return a.lobbyPort }

// LobbyInitMode returns the lobby init mode.
func (a *Adapter) LobbyInitMode() int {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.lobbyInitMode
}

// SetLobbyInitMode sets the lobby init mode (thread-safe).
func (a *Adapter) SetLobbyInitMode(mode int) {
	a.mu.Lock()
	a.lobbyInitMode = mode
	a.mu.Unlock()
}

// SetHost marks this adapter as the lobby host (thread-safe).
// Called when hostGame is received.
func (a *Adapter) SetHost() {
	a.mu.Lock()
	a.isHost = true
	a.mu.Unlock()
}

// IsHost returns true if this adapter is the lobby host (thread-safe).
func (a *Adapter) IsHost() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.isHost
}

// SetRelayMode sets the lobby-wide relay mode flag (thread-safe).
// Called once in hostGame or joinGame. All subsequent connectToPeer calls
// use this flag instead of per-peer HTTP checks.
func (a *Adapter) SetRelayMode(relay bool) {
	a.mu.Lock()
	a.relayMode = relay
	a.mu.Unlock()
}

// RelayMode returns true if this lobby uses dedicated relay mode (thread-safe).
func (a *Adapter) RelayMode() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.relayMode
}

// signalGameDisconnected signals that the game has disconnected from GPGNet.
// Only triggers adapter shutdown when Run() is active (channel is non-nil).
func (a *Adapter) signalGameDisconnected() {
	if a.gameDisconnectedCh != nil {
		select {
		case a.gameDisconnectedCh <- struct{}{}:
		default:
		}
	}
}

// Run starts all adapter components. Blocks until context cancelled or quit.
//
// Startup is intentionally non-blocking: GPGNet and RPC servers are started
// immediately so ForgedAlliance.exe and the FAF client can connect without
// delay. Relay transport probing (UDP probe / TCP fallback) and HTTP
// registration happen in background goroutines.
func (a *Adapter) Run(parentCtx context.Context) error {
	a.ctx, a.cancel = context.WithCancel(parentCtx)
	defer a.cancel()

	a.gameDisconnectedCh = make(chan struct{}, 1)

	// 1. Create peer manager
	a.peerMgr = NewPeerManager(uint32(a.playerID), a.logger)

	// 2. Create relay connection (UDP socket only, transport not yet decided)
	var err error
	a.relayConn, err = NewRelayConnection(a.cfg.RelayServer, uint32(a.playerID), a.peerMgr, a.logger)
	if err != nil {
		return fmt.Errorf("creating relay connection: %w", err)
	}
	defer a.relayConn.Close()
	a.peerMgr.SetRelay(a.relayConn)

	// 3. Allocate a separate lobby port for the game.
	// MUST NOT reuse relayConn's port — the game binds its own UDP socket
	// to the lobby port, and having two sockets on the same port causes
	// packets to be delivered to the wrong socket.
	if a.cfg.LobbyPort == 0 {
		tmp, err := net.ListenPacket("udp4", "127.0.0.1:0")
		if err != nil {
			return fmt.Errorf("allocating lobby port: %w", err)
		}
		a.lobbyPort = tmp.LocalAddr().(*net.UDPAddr).Port
		tmp.Close() // free the port so the game can bind to it
	} else {
		a.lobbyPort = a.cfg.LobbyPort
	}

	// 4. Start GPGNet server immediately so ForgedAlliance.exe can connect
	//    without waiting for relay transport probing to finish.
	a.gpgnet, err = NewGPGNetServer(a.cfg.GPGNetPort, a, a.logger)
	if err != nil {
		return fmt.Errorf("creating GPGNet server: %w", err)
	}
	defer a.gpgnet.Close()

	// 5. Start RPC server immediately so the FAF client can talk to us
	//    and query status/ports without waiting for relay transport.
	rpcHandler := NewRPCHandler(a, a.logger)
	a.rpc, err = NewRPCServer(a.cfg.RPCPort, rpcHandler, a.logger)
	if err != nil {
		return fmt.Errorf("creating RPC server: %w", err)
	}
	defer a.rpc.Close()
	a.rpcPort = a.rpc.Port()

	// 5b. Always start debug window — useful for diagnosing connection issues.
	{
		dbg, err := NewDebugServer(a)
		if err != nil {
			a.logger.Warn("debug server failed to start", "error", err)
		} else {
			defer dbg.Close()
			debugURL := fmt.Sprintf("http://127.0.0.1:%d", dbg.Port())
			a.logger.Info("Debug window available", "url", debugURL)
			OpenBrowser(debugURL)
		}
	}

	a.logger.Info("Adapter started",
		"player_id", a.playerID,
		"login", a.login,
		"rpc_port", a.rpcPort,
		"gpgnet_port", a.gpgnet.Port(),
		"lobby_port", a.lobbyPort,
		"relay_server", a.cfg.RelayServer,
		"game_id", a.cfg.GameID,
		"use_official_faf", a.cfg.UseOfficialFAF,
	)

	// 6. Start relay loops. ReceiveLoop waits internally for transport to be
	//    decided (MarkTransportReady) before it begins reading from the relay.
	go a.relayConn.ReceiveLoop(a.ctx)
	go a.relayConn.RunKeepalive(a.ctx)
	go a.relayConn.RunStats(a.ctx)
	go a.relayConn.RunHealthMonitor(a.ctx, a.onRelayHealthChanged)
	go a.gpgnet.Run(a.ctx)
	go a.rpc.Run(a.ctx)

	// 7. Background: HTTP health check + relay registration.
	//    Non-blocking — relay server being offline must never prevent the
	//    adapter from starting or the game from entering the lobby screen.
	//    Skipped entirely when UseOfficialFAF is set: the host deliberately
	//    stays off the relay so all peers fall back to standard ICE.
	if a.cfg.UseOfficialFAF {
		a.logger.Info("Official FAF mode: skipping relay registration and transport probe")
		a.relayConn.MarkTransportReady() // unblock ReceiveLoop immediately
	} else {
		go a.ensureRelayReady()

		// 8. Background: probe relay transport (UDP → TCP fallback if needed).
		//    ReceiveLoop blocks until this goroutine calls MarkTransportReady.
		//    Non-blocking — if both transports fail the adapter continues with
		//    UDP mode (relay simply won't forward packets until connectivity
		//    is restored via keepalive).
		go a.probeAndSetupTransport(a.ctx)
	}

	// 9. Shut down the adapter when the game disconnects from GPGNet.
	//    Without a running game the adapter has no purpose and would
	//    otherwise hang forever (e.g. when the FAF client kills
	//    ForgedAlliance.exe but never sends a "quit" RPC).
	go func() {
		select {
		case <-a.gameDisconnectedCh:
			a.logger.Info("Game disconnected, shutting down adapter")
			a.cancel()
		case <-a.ctx.Done():
		}
	}()

	// Block until done
	<-a.ctx.Done()

	// Cleanup: close peers first, then deregister from relay with a short
	// deadline so we don't block shutdown if the relay server is unreachable.
	a.peerMgr.CloseAll()
	a.leaveRelay()
	a.logger.Info("Adapter stopped")
	return nil
}

// probeAndSetupTransport runs in a background goroutine and decides whether to
// use UDP or TCP for relay communication. It calls MarkTransportReady when
// done so that ReceiveLoop can start reading from the chosen transport.
func (a *Adapter) probeAndSetupTransport(ctx context.Context) {
	// Always mark ready when this function exits, even on error, so that
	// ReceiveLoop is never left waiting forever.
	defer a.relayConn.MarkTransportReady()

	if a.cfg.ForceTCP {
		// Force TCP: skip UDP probe entirely.
		tcpAddr := a.cfg.RelayTCPServer
		if tcpAddr == "" {
			tcpAddr = a.cfg.RelayServer
		}
		a.logger.Info("Force-TCP mode: connecting via TCP", "addr", tcpAddr)
		if err := a.relayConn.SwitchToTCP(ctx, tcpAddr); err != nil {
			a.logger.Warn("Force-TCP connection failed, adapter will run without relay connectivity", "error", err)
		} else {
			if err := a.relayConn.SendHello(); err != nil {
				a.logger.Warn("Failed to send TCP hello to relay", "error", err)
			}
		}
	} else if a.cfg.TCPFallback {
		// Probe UDP; if it fails within 5 s, fall back to TCP.
		// Run probe in a sub-goroutine so we can abort via ctx.Done().
		type probeResult struct{ err error }
		probeCh := make(chan probeResult, 1)
		go func() {
			probeCh <- probeResult{err: a.relayConn.ProbeRelayUDP()}
		}()

		select {
		case <-ctx.Done():
			// Adapter shutting down during probe — exit without marking TCP.
			return
		case res := <-probeCh:
			if res.err != nil {
				a.logger.Warn("UDP probe failed, attempting TCP fallback", "error", res.err)
				tcpAddr := a.cfg.RelayTCPServer
				if tcpAddr == "" {
					tcpAddr = a.cfg.RelayServer
				}
				if err := a.relayConn.SwitchToTCP(ctx, tcpAddr); err != nil {
					a.logger.Error("TCP fallback also failed, continuing with UDP", "error", err)
				} else {
					if err := a.relayConn.SendHello(); err != nil {
						a.logger.Warn("Failed to send TCP hello to relay", "error", err)
					}
				}
			}
			// UDP probe succeeded — stay in UDP mode (already the default).
		}
	} else {
		// No probing — just send a UDP hello so relay learns our address.
		if err := a.relayConn.SendHello(); err != nil {
			a.logger.Warn("Failed to send initial hello to relay", "error", err)
		}
	}

	a.logger.Info("Relay transport decided", "transport", a.relayConn.Transport())
}

// httpClient is a shared HTTP client with timeouts to prevent hanging
// on unresponsive relay servers.
var httpClient = &http.Client{
	Timeout: 10 * time.Second,
}

// checkRelayHealth calls GET /health on the relay server to verify it's running.
func (a *Adapter) checkRelayHealth() error {
	resp, err := httpClient.Get(a.cfg.RelayHTTP + "/health")
	if err != nil {
		return fmt.Errorf("relay server unreachable at %s: %w", a.cfg.RelayHTTP, err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("relay server unhealthy (HTTP %d)", resp.StatusCode)
	}
	a.logger.Info("Relay server HTTP health check passed", "url", a.cfg.RelayHTTP)
	return nil
}

// registerWithRelay registers this player with the relay server's HTTP API.
func (a *Adapter) registerWithRelay() error {
	sessionID := fmt.Sprintf("%d", a.cfg.GameID)

	// Try to create session (may already exist — 409 is OK)
	createBody, _ := json.Marshal(map[string]interface{}{
		"session_id":  sessionID,
		"max_players": 32,
	})
	resp, err := a.doRelayHTTP("POST", a.cfg.RelayHTTP+"/sessions", createBody)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusConflict {
		return fmt.Errorf("create session: unexpected status %d", resp.StatusCode)
	}

	// Join session
	joinBody, _ := json.Marshal(map[string]interface{}{
		"player_id": a.playerID,
		"login":     a.login,
	})
	resp, err = a.doRelayHTTP("POST", fmt.Sprintf("%s/sessions/%s/join", a.cfg.RelayHTTP, sessionID), joinBody)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("join session: unexpected status %d", resp.StatusCode)
	}

	a.logger.Info("Registered with relay server", "session", sessionID)
	return nil
}

// leaveRelay deregisters this player from the relay session via HTTP.
// Called on adapter shutdown so the slot is freed immediately (instead of
// waiting up to 5 minutes for the reaper).
// Uses a short deadline so adapter shutdown is never delayed significantly
// when the relay server is unreachable.
func (a *Adapter) leaveRelay() {
	if a.cfg.RelayHTTP == "" || !a.relayMode {
		return
	}
	// 3-second deadline: fast enough to not delay the game closing, yet long
	// enough to succeed on a healthy network.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	sessionID := fmt.Sprintf("%d", a.cfg.GameID)
	leaveBody, _ := json.Marshal(map[string]interface{}{
		"player_id": a.playerID,
	})
	resp, err := a.doRelayHTTPWithCtx(ctx, "POST",
		fmt.Sprintf("%s/sessions/%s/leave", a.cfg.RelayHTTP, sessionID),
		leaveBody)
	if err != nil {
		a.logger.Warn("Failed to deregister from relay (leave)", "error", err)
		return
	}
	resp.Body.Close()
	a.logger.Info("Deregistered from relay session", "session", sessionID)
}

// doRelayHTTP performs an HTTP request to the relay server, adding the API key if configured.
func (a *Adapter) doRelayHTTP(method, url string, body []byte) (*http.Response, error) {
	req, err := http.NewRequest(method, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if a.cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+a.cfg.APIKey)
	}
	return httpClient.Do(req)
}

// doRelayHTTPWithCtx is like doRelayHTTP but honours the provided context for
// cancellation and deadline. Used for shutdown-path requests where we need to
// bound the wait time regardless of httpClient.Timeout.
func (a *Adapter) doRelayHTTPWithCtx(ctx context.Context, method, url string, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if a.cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+a.cfg.APIKey)
	}
	return httpClient.Do(req)
}

// onRelayHealthChanged is called when the relay transitions between reachable/unreachable.
func (a *Adapter) onRelayHealthChanged(reachable bool) {
	if reachable {
		a.NotifyRelayStateChanged("Connected")
	} else {
		a.NotifyRelayStateChanged("Disconnected")
	}
}

// notifyPeerConnectedWhenRelayReady waits for the relay server to be confirmed
// reachable, then sends "Connected" notifications to the FAF client.
// If the relay never becomes reachable (server down), the notifications are
// never sent and the FAF client stays on the "Connecting…" screen.
// Intended to run as a goroutine.
func (a *Adapter) notifyPeerConnectedWhenRelayReady(ctx context.Context, pid int) {
	if a.relayConn == nil || a.relayConn.WaitRelayConfirmed(ctx) {
		a.NotifyIceConnectionStateChanged(a.playerID, pid, "Connected")
		a.NotifyConnected(a.playerID, pid, true)
	}
}

// NotifyRelayStateChanged sends onRelayConnectionStateChanged to FAF client.
func (a *Adapter) NotifyRelayStateChanged(state string) {
	if a.rpc != nil {
		a.rpc.SendNotification("onRelayConnectionStateChanged", []interface{}{state})
	}
}

// NotifyConnectionStateChanged sends onConnectionStateChanged to FAF client.
func (a *Adapter) NotifyConnectionStateChanged(state string) {
	if a.rpc != nil {
		a.rpc.SendNotification("onConnectionStateChanged", []interface{}{state})
	}
}

// NotifyGpgNetMessage sends onGpgNetMessageReceived to FAF client.
func (a *Adapter) NotifyGpgNetMessage(header string, chunks []interface{}) {
	if a.rpc != nil {
		a.rpc.SendNotification("onGpgNetMessageReceived", []interface{}{header, chunks})
	}
}

// NotifyIceConnectionStateChanged sends onIceConnectionStateChanged to FAF client.
func (a *Adapter) NotifyIceConnectionStateChanged(localID, remoteID int, state string) {
	if a.rpc != nil {
		a.rpc.SendNotification("onIceConnectionStateChanged", []interface{}{
			float64(localID), float64(remoteID), state,
		})
	}
}

// NotifyConnected sends onConnected to FAF client.
func (a *Adapter) NotifyConnected(localID, remoteID int, connected bool) {
	if a.rpc != nil {
		a.rpc.SendNotification("onConnected", []interface{}{
			float64(localID), float64(remoteID), connected,
		})
	}
}

// ForceICEReconnect triggers a manual ICE restart for the given player.
// Called from the debug UI. Only works for ICE peers, not dedicated relay peers.
func (a *Adapter) ForceICEReconnect(playerID int) error {
	icePeer := a.peerMgr.GetICEPeer(playerID)
	if icePeer == nil {
		return fmt.Errorf("no ICE peer for player %d", playerID)
	}
	a.logger.Info("Manual ICE reconnect triggered from debug UI", "playerId", playerID)
	icePeer.ForceRestart()
	return nil
}

// SetICEServers stores the ICE server configuration from the FAF client.
func (a *Adapter) SetICEServers(servers []webrtc.ICEServer) {
	a.mu.Lock()
	a.iceServers = servers
	a.mu.Unlock()
}

// GetICEServers returns the stored ICE server configuration.
func (a *Adapter) GetICEServers() []webrtc.ICEServer {
	a.mu.RLock()
	defer a.mu.RUnlock()
	result := make([]webrtc.ICEServer, len(a.iceServers))
	copy(result, a.iceServers)
	return result
}

// sendIceMsg sends an onIceMsg notification to the FAF client.
func (a *Adapter) sendIceMsg(remotePlayerID int, msgJSON string) {
	if a.rpc != nil {
		a.rpc.SendNotification("onIceMsg", []interface{}{
			float64(a.playerID), float64(remotePlayerID), msgJSON,
		})
	}
}

// isHostOnRelay checks whether a given player is registered in the relay
// session for this game. Returns true if the relay server has the player
// in the session (meaning the remote player uses our relay adapter).
// This check is independent of the local UseOfficialFAF setting — the local
// config says nothing about whether the REMOTE player uses the relay.
func (a *Adapter) isHostOnRelay(hostPlayerID int) bool {
	if a.cfg.RelayHTTP == "" {
		return false
	}

	sessionID := fmt.Sprintf("%d", a.cfg.GameID)
	url := fmt.Sprintf("%s/sessions/%s", a.cfg.RelayHTTP, sessionID)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		a.logger.Warn("isHostOnRelay: failed to create request", "error", err)
		return false
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		a.logger.Debug("isHostOnRelay: relay server unreachable, using ICE mode", "error", err)
		return false
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		a.logger.Debug("isHostOnRelay: no relay session found, using ICE mode", "session", sessionID)
		return false
	}
	if resp.StatusCode != http.StatusOK {
		a.logger.Warn("isHostOnRelay: unexpected HTTP status", "status", resp.StatusCode)
		return false
	}

	var session struct {
		PlayerIDs []int `json:"player_ids"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&session); err != nil {
		a.logger.Warn("isHostOnRelay: failed to decode response", "error", err)
		return false
	}

	for _, id := range session.PlayerIDs {
		if id == hostPlayerID {
			a.logger.Info("host is on relay, using relay mode", "host_player_id", hostPlayerID, "session", sessionID)
			return true
		}
	}

	a.logger.Info("host not on relay, using ICE mode", "host_player_id", hostPlayerID, "session", sessionID)
	return false
}

// ensureRelayReady registers this adapter with the relay server and sends
// a hello packet so the server learns our UDP address. Uses sync.Once so
// it is safe to call from multiple goroutines and only executes once.
// This is called:
//   - During startup (Run) when UseOfficialFAF=false
//   - On-demand in joinGame/connectToPeer when a relay peer is needed but
//     UseOfficialFAF=true initially skipped registration
func (a *Adapter) ensureRelayReady() {
	a.relaySetupOnce.Do(func() {
		if a.cfg.RelayHTTP == "" {
			return
		}
		a.logger.Info("ensureRelayReady: registering with relay server")
		if err := a.checkRelayHealth(); err != nil {
			a.logger.Warn("ensureRelayReady: health check failed", "error", err)
		}
		if err := a.registerWithRelay(); err != nil {
			a.logger.Warn("ensureRelayReady: registration failed", "error", err)
			return
		}
		// Send hello so relay learns our UDP address immediately
		// (don't wait for next 15s keepalive cycle).
		if err := a.relayConn.SendHello(); err != nil {
			a.logger.Warn("ensureRelayReady: hello failed", "error", err)
		}
	})
}
