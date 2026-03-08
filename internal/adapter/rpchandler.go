package adapter

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"fafsturmudprelay/internal/protocol"
	"github.com/pion/webrtc/v4"
)

const (
	// maxLoginLen is the maximum allowed length of a player login string.
	maxLoginLen = 64
	// maxMapNameLen is the maximum allowed length of a map name string.
	maxMapNameLen = 256
)

// RPCHandler dispatches incoming JSON-RPC requests.
type RPCHandler struct {
	adapter *Adapter
	logger  *slog.Logger
}

// NewRPCHandler creates a new RPC handler.
func NewRPCHandler(adapter *Adapter, logger *slog.Logger) *RPCHandler {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &RPCHandler{
		adapter: adapter,
		logger:  logger,
	}
}

// HandleRequest dispatches a JSON-RPC request and returns the result.
func (h *RPCHandler) HandleRequest(req *protocol.JSONRPCRequest) (interface{}, error) {
	h.logger.Debug("RPC request", "method", req.Method, "id", req.ID)

	switch req.Method {
	case "hostGame":
		return h.hostGame(req.Params)
	case "joinGame":
		return h.joinGame(req.Params)
	case "connectToPeer":
		return h.connectToPeer(req.Params)
	case "disconnectFromPeer":
		return h.disconnectFromPeer(req.Params)
	case "setLobbyInitMode":
		return h.setLobbyInitMode(req.Params)
	case "iceMsg":
		return h.iceMsg(req.Params)
	case "sendToGpgNet":
		return h.sendToGpgNet(req.Params)
	case "setIceServers":
		return h.setIceServers(req.Params)
	case "status":
		return h.status()
	case "quit":
		return h.quit()
	default:
		return nil, fmt.Errorf("unknown method: %s", req.Method)
	}
}

func (h *RPCHandler) hostGame(params []json.RawMessage) (interface{}, error) {
	if len(params) < 1 {
		return nil, fmt.Errorf("hostGame requires 1 param (mapName)")
	}
	var mapName string
	if err := json.Unmarshal(params[0], &mapName); err != nil {
		return nil, fmt.Errorf("hostGame param: %w", err)
	}
	if len(mapName) > maxMapNameLen {
		return nil, fmt.Errorf("hostGame mapName too long (max %d)", maxMapNameLen)
	}

	h.logger.Info("hostGame", "map", mapName)
	h.adapter.SetHost()

	// Decide relay mode for the entire lobby: relay when not using official FAF
	// and not in matchmaking (auto) mode.
	useRelay := !h.adapter.cfg.UseOfficialFAF && h.adapter.LobbyInitMode() != protocol.LobbyInitModeAuto
	h.adapter.SetRelayMode(useRelay)

	useICE := !useRelay
	if useICE {
		if h.adapter.LobbyInitMode() == protocol.LobbyInitModeAuto {
			h.logger.Info("hostGame: ICE mode (matchmaking detected, lobbyInitMode=auto)")
		} else {
			h.logger.Info("hostGame: ICE mode (UseOfficialFAF=true)")
		}
		// ICE mode: send HostGame immediately (no relay dependency).
		h.adapter.gpgnet.SendToGameQueued(protocol.CmdHostGame, []interface{}{mapName})
		h.adapter.NotifyIceConnectionStateChanged(h.adapter.playerID, h.adapter.playerID, "Connected")
		h.adapter.NotifyConnected(h.adapter.playerID, h.adapter.playerID, true)
	} else {
		h.logger.Info("hostGame: relay mode (UseOfficialFAF=false)")
		// Tell FAF client we're waiting for relay confirmation (self-connection state).
		h.adapter.NotifyIceConnectionStateChanged(h.adapter.playerID, h.adapter.playerID, "Connecting")
		h.adapter.NotifyConnected(h.adapter.playerID, h.adapter.playerID, false)

		// Delay the HostGame command to the game until the relay server is confirmed
		// reachable. The game stays in lobby-wait state until then, keeping the FAF
		// client on the "Connecting…" screen. If the relay never responds, the game
		// never starts hosting.
		go func() {
			ctx := h.adapter.ctx
			if h.adapter.relayConn == nil || h.adapter.relayConn.WaitRelayConfirmed(ctx) {
				h.adapter.gpgnet.SendToGameQueued(protocol.CmdHostGame, []interface{}{mapName})
				h.adapter.NotifyIceConnectionStateChanged(h.adapter.playerID, h.adapter.playerID, "Connected")
				h.adapter.NotifyConnected(h.adapter.playerID, h.adapter.playerID, true)
			}
		}()
	}
	return nil, nil
}

func (h *RPCHandler) joinGame(params []json.RawMessage) (interface{}, error) {
	if len(params) < 2 {
		return nil, fmt.Errorf("joinGame requires 2 params (login, playerId)")
	}
	var login string
	var playerID float64
	if err := json.Unmarshal(params[0], &login); err != nil {
		return nil, fmt.Errorf("joinGame login param: %w", err)
	}
	if err := json.Unmarshal(params[1], &playerID); err != nil {
		return nil, fmt.Errorf("joinGame playerId param: %w", err)
	}

	if len(login) > maxLoginLen {
		return nil, fmt.Errorf("joinGame login too long (max %d)", maxLoginLen)
	}
	pid := int(playerID)
	if pid <= 0 {
		return nil, fmt.Errorf("joinGame invalid playerId: %d", pid)
	}
	h.logger.Info("joinGame", "login", login, "playerId", pid)

	// Guard against duplicate calls: if a peer already exists (e.g. a
	// prior connectToPeer for the same player), skip creating another one.
	// For relay peers: re-send JoinGame to the game so that a rejoining
	// player's game re-establishes the data flow (the game may have timed
	// out the old connection internally).
	if existingRelay := h.adapter.peerMgr.GetPeer(pid); existingRelay != nil {
		h.logger.Info("joinGame: relay peer already exists, re-sending JoinGame to game", "playerId", pid)
		port := existingRelay.LocalPort()
		h.adapter.gpgnet.SendToGameQueued(protocol.CmdJoinGame, []interface{}{
			fmt.Sprintf("127.0.0.1:%d", port),
			login,
			int32(pid),
		})
		return nil, nil
	}
	if existingICE := h.adapter.peerMgr.GetICEPeer(pid); existingICE != nil {
		h.logger.Debug("joinGame: ICE peer already exists, skipping duplicate", "playerId", pid)
		return nil, nil
	}

	// Detect: is the host on our relay, or do we need ICE fallback?
	// In matchmaking (lobbyInitMode=auto) always use ICE — the opponent
	// likely does not have the Sturm relay adapter.
	// joinGame is only called on the PEER side — check if the HOST is on relay.
	onRelay := false
	if h.adapter.LobbyInitMode() != protocol.LobbyInitModeAuto {
		onRelay = h.adapter.isHostOnRelay(pid)
	}
	h.adapter.SetRelayMode(onRelay)

	if onRelay {
		// Relay mode — host is on our dedicated relay server.
		// Ensure we are registered with the relay server. This is a no-op if
		// registration already happened during startup, but covers the case
		// where UseOfficialFAF=true initially skipped registration.
		h.adapter.ensureRelayReady()
		h.logger.Info("relay mode for peer (host on relay)", "playerId", pid)
		peer, err := h.adapter.peerMgr.AddPeer(pid, login)
		if err != nil {
			return nil, fmt.Errorf("creating relay peer: %w", err)
		}
		peer.Start(h.adapter.ctx)

		port := peer.LocalPort()
		h.adapter.gpgnet.SendToGameQueued(protocol.CmdJoinGame, []interface{}{
			fmt.Sprintf("127.0.0.1:%d", port),
			login,
			int32(pid),
		})
		go h.adapter.notifyPeerConnectedWhenRelayReady(h.adapter.ctx, pid)
	} else {
		// ICE fallback mode
		h.logger.Info("ICE mode for peer", "playerId", pid)
		iceServers := h.adapter.GetICEServers()
		onMsg := func(msg string) { h.adapter.sendIceMsg(pid, msg) }
		var lastConnectedAt time.Time
		onConnected := func() {
			lastConnectedAt = time.Now()
			h.adapter.NotifyIceConnectionStateChanged(h.adapter.playerID, pid, "Connected")
			h.adapter.NotifyConnected(h.adapter.playerID, pid, true)
		}

		icePeer, err := h.adapter.peerMgr.AddICEPeer(pid, login, false, iceServers, onMsg, onConnected,
			h.iceRestartCallback(pid, login, false, onMsg, onConnected, &lastConnectedAt))
		if err != nil {
			return nil, fmt.Errorf("creating ICE peer: %w", err)
		}
		icePeer.Start(h.adapter.ctx)

		port := icePeer.LocalPort()
		h.adapter.gpgnet.SendToGameQueued(protocol.CmdJoinGame, []interface{}{
			fmt.Sprintf("127.0.0.1:%d", port),
			login,
			int32(pid),
		})

		// Notify FAF "Connecting" immediately; "Connected" fires when ICE connects
		h.adapter.NotifyIceConnectionStateChanged(h.adapter.playerID, pid, "Connecting")
		h.adapter.NotifyConnected(h.adapter.playerID, pid, false)
	}

	return nil, nil
}

func (h *RPCHandler) connectToPeer(params []json.RawMessage) (interface{}, error) {
	if len(params) < 3 {
		return nil, fmt.Errorf("connectToPeer requires 3 params (login, playerId, offer)")
	}
	var login string
	var playerID float64
	var offer bool
	if err := json.Unmarshal(params[0], &login); err != nil {
		return nil, fmt.Errorf("connectToPeer login param: %w", err)
	}
	if err := json.Unmarshal(params[1], &playerID); err != nil {
		return nil, fmt.Errorf("connectToPeer playerId param: %w", err)
	}
	if err := json.Unmarshal(params[2], &offer); err != nil {
		return nil, fmt.Errorf("connectToPeer offer param: %w", err)
	}

	if len(login) > maxLoginLen {
		return nil, fmt.Errorf("connectToPeer login too long (max %d)", maxLoginLen)
	}
	pid := int(playerID)
	if pid <= 0 {
		return nil, fmt.Errorf("connectToPeer invalid playerId: %d", pid)
	}
	h.logger.Info("connectToPeer", "login", login, "playerId", pid, "offer", offer)

	// Check game state — abort if LAUNCHING or ENDED
	state := h.adapter.gpgnet.GameState()
	if state == protocol.GameStateLaunching || state == protocol.GameStateEnded {
		h.logger.Warn("connectToPeer ignored, game state", "state", state)
		return nil, nil
	}

	// --- Deduplication: check for existing peer ---
	// The FAF client may call connectToPeer multiple times for the same player
	// (e.g. 3× offer=true then 1× offer=false).  Sending duplicate ConnectToPeer
	// commands to the game can disrupt established connections: ForgedAlliance may
	// reset the UDP socket, killing the data flow while ICE still reports
	// "Connected" — which manifests as "Connecting…" in the lobby.
	//
	// Exception: relay peers always re-send ConnectToPeer to the game.  When a
	// player disconnects and rejoins the same lobby, the host's adapter still has
	// the old relay peer (no disconnectFromPeer was received).  Without re-sending
	// ConnectToPeer, the game never learns about the rejoin and times out.
	if existingRelay := h.adapter.peerMgr.GetPeer(pid); existingRelay != nil {
		h.logger.Info("connectToPeer: relay peer already exists, re-sending ConnectToPeer to game", "playerId", pid)
		port := existingRelay.LocalPort()
		h.adapter.gpgnet.SendToGameQueued(protocol.CmdConnectToPeer, []interface{}{
			fmt.Sprintf("127.0.0.1:%d", port),
			login,
			int32(pid),
		})
		return nil, nil
	}
	if existingICE := h.adapter.peerMgr.GetICEPeer(pid); existingICE != nil {
		if existingICE.Offerer == offer {
			h.logger.Debug("connectToPeer: ICE peer already exists with same role, skipping", "playerId", pid, "offer", offer)
			return nil, nil
		}
		// The FAF client changed the offer role for an existing peer.
		// We must replace the ICE agent with the correct role so both sides agree
		// on who Dials (controlling) and who Accepts (controlled).
		h.logger.Info("connectToPeer: offer role changed, replacing ICE peer",
			"playerId", pid, "oldOffer", existingICE.Offerer, "newOffer", offer)

		iceServers := h.adapter.GetICEServers()
		onMsg := func(msg string) { h.adapter.sendIceMsg(pid, msg) }
		var lastConnectedAt time.Time
		onConnected := func() {
			lastConnectedAt = time.Now()
			h.adapter.NotifyIceConnectionStateChanged(h.adapter.playerID, pid, "Connected")
			h.adapter.NotifyConnected(h.adapter.playerID, pid, true)
		}

		newPeer, err := h.adapter.peerMgr.ReplaceICEPeer(
			pid, login, offer, iceServers, onMsg, onConnected,
			h.iceRestartCallback(pid, login, offer, onMsg, onConnected, &lastConnectedAt),
			nil, // no initial ICE message — wait for fresh signaling
		)
		if err != nil {
			h.logger.Error("connectToPeer: failed to replace ICE peer", "playerId", pid, "error", err)
			return nil, nil
		}
		newPeer.Start(h.adapter.ctx)

		// Don't send ConnectToPeer to game — the port is unchanged (ReplaceICEPeer
		// reuses the UDP socket).  Just update FAF client state.
		h.adapter.NotifyIceConnectionStateChanged(h.adapter.playerID, pid, "Connecting")
		h.adapter.NotifyConnected(h.adapter.playerID, pid, false)
		return nil, nil
	}

	// --- No existing peer: create fresh ---

	// Use the lobby-wide relay mode decided in hostGame/joinGame.
	// No per-peer HTTP checks — the decision was already made.
	onRelay := h.adapter.RelayMode()

	if onRelay {
		// Relay mode — ensure we are registered with the relay server.
		h.adapter.ensureRelayReady()
		h.logger.Info("relay mode for peer", "playerId", pid)
		peer, err := h.adapter.peerMgr.AddPeer(pid, login)
		if err != nil {
			return nil, fmt.Errorf("creating relay peer: %w", err)
		}
		peer.Start(h.adapter.ctx)

		port := peer.LocalPort()
		h.adapter.gpgnet.SendToGameQueued(protocol.CmdConnectToPeer, []interface{}{
			fmt.Sprintf("127.0.0.1:%d", port),
			login,
			int32(pid),
		})
		go h.adapter.notifyPeerConnectedWhenRelayReady(h.adapter.ctx, pid)
	} else {
		// ICE fallback mode
		h.logger.Info("ICE mode for peer", "playerId", pid, "offer", offer)
		iceServers := h.adapter.GetICEServers()
		onMsg := func(msg string) { h.adapter.sendIceMsg(pid, msg) }
		var lastConnectedAt time.Time
		onConnected := func() {
			lastConnectedAt = time.Now()
			h.adapter.NotifyIceConnectionStateChanged(h.adapter.playerID, pid, "Connected")
			h.adapter.NotifyConnected(h.adapter.playerID, pid, true)
		}

		icePeer, err := h.adapter.peerMgr.AddICEPeer(pid, login, offer, iceServers, onMsg, onConnected,
			h.iceRestartCallback(pid, login, offer, onMsg, onConnected, &lastConnectedAt))
		if err != nil {
			return nil, fmt.Errorf("creating ICE peer: %w", err)
		}
		icePeer.Start(h.adapter.ctx)

		port := icePeer.LocalPort()
		h.adapter.gpgnet.SendToGameQueued(protocol.CmdConnectToPeer, []interface{}{
			fmt.Sprintf("127.0.0.1:%d", port),
			login,
			int32(pid),
		})

		// Notify FAF "Connecting" immediately
		h.adapter.NotifyIceConnectionStateChanged(h.adapter.playerID, pid, "Connecting")
		h.adapter.NotifyConnected(h.adapter.playerID, pid, false)
	}

	return nil, nil
}

// iceRestartCallback builds the onRestart callback for an ICE peer.
//
// Two cases are handled:
//
//   - Remote-triggered restart: the remote java-ice-adapter sent new ICE credentials
//     (msg.Ufrag != ""). The triggering message is pre-loaded into the new peer so
//     signaling continues immediately without waiting for a second message.
//
//   - Self-initiated restart: we are the offerer and our Dial() timed out
//     (msg.Ufrag == ""). We create a fresh peer, send our new credentials to FAF, and
//     wait for the remote answerer to respond. A 2-second backoff gives the remote time
//     to tear down its old session before we flood it with new credentials.
//
// Restarts are capped at 5 consecutive failures per peer to avoid infinite
// retry loops.  The backoff grows exponentially (1 s, 2 s, 4 s, 8 s, 16 s) so
// repeated failures — e.g. caused by a VPN with symmetric NAT — do not hammer
// the signaling server.
//
// The failure counter resets to 0 only when a connection has been stable for at
// least 60 seconds.  This prevents VPN-induced "connect → 15 s drop → restart →
// connect → reset → repeat" loops from exhausting the retry budget while still
// allowing recovery after a genuinely stable long session.
//
// pLastConnectedAt must be a pointer to a variable that is ALSO updated by the
// initial peer's onConnected callback (at the callsite).  Sharing the variable
// means that even the very first restart can detect a short-lived connection
// and switch to relay-only mode, instead of making an unnecessary second P2P
// attempt that will drop again after ~15 s.
func (h *RPCHandler) iceRestartCallback(pid int, login string, offerer bool, onMsg func(string), onConnected func(), pLastConnectedAt *time.Time) func(fafIceMsg) {
	// mu protects closure state that can be accessed concurrently when a
	// self-initiated restart (from ICE Failed) and a remote-triggered restart
	// (from HandleIceMsg) run in parallel.  The lock is released before the
	// backoff sleep so the remote callback is not blocked.
	var mu sync.Mutex
	consecutiveFailures := 0
	// sinceConnectedSnapshot freezes the elapsed time between connection
	// establishment and the first restart.  Without this snapshot,
	// time.Since(*pLastConnectedAt) keeps growing across signaling timeouts
	// (30 s each), eventually exceeding the 60 s "stable connection" threshold
	// and resetting the failure counter — even for a 4-second connection.
	var sinceConnectedSnapshot time.Duration
	var cb func(fafIceMsg)
	cb = func(msg fafIceMsg) {
		// Immediately notify the FAF client that the connection dropped.
		// Without this, the client keeps showing "Connected" while the game
		// shows "Connecting…" because the game-level handshake is stale.
		h.adapter.NotifyIceConnectionStateChanged(h.adapter.playerID, pid, "Disconnected")
		h.adapter.NotifyConnected(h.adapter.playerID, pid, false)

		// --- Begin protected section (shared closure state) ---
		mu.Lock()

		// Manual restart from debug UI: reset failure counter and skip backoff.
		if msg.Manual {
			consecutiveFailures = 0
			sinceConnectedSnapshot = 0
			h.logger.Info("ICE restart: manual reconnect from debug UI", "playerId", pid)
		}

		// On the first restart after a successful connection, snapshot the
		// elapsed time.  Subsequent restarts (e.g. after signaling timeouts)
		// reuse the snapshot so the elapsed time does not inflate.
		if !pLastConnectedAt.IsZero() {
			sinceConnectedSnapshot = time.Since(*pLastConnectedAt)
			*pLastConnectedAt = time.Time{}
		}

		// Reset failure counter only if the last connection was stable for ≥60 s.
		// A brief connect-then-drop (e.g. VPN flapping) must NOT reset the counter
		// so the exponential backoff and retry cap remain effective.
		if sinceConnectedSnapshot >= 60*time.Second {
			consecutiveFailures = 0
			sinceConnectedSnapshot = 0 // consumed — don't reset again
		}

		consecutiveFailures++
		localCF := consecutiveFailures
		localSnapshot := sinceConnectedSnapshot
		mu.Unlock()
		// --- End protected section ---

		if localCF > 5 {
			h.logger.Warn("ICE restart limit reached, giving up", "playerId", pid, "consecutiveFailures", localCF-1)
			return
		}

		selfInitiated := msg.Ufrag == ""
		if selfInitiated && !msg.Manual {
			// Exponential backoff: 1 s, 2 s, 4 s, 8 s, 16 s (capped).
			// With the fast CheckInterval (20 ms) relay pairs are now tried on
			// the first attempt, so restarts are rare.  A 1 s initial backoff
			// still gives the remote time to tear down its old session.
			backoff := time.Duration(1<<localCF) * 500 * time.Millisecond
			if backoff > 16*time.Second {
				backoff = 16 * time.Second
			}
			h.logger.Info("ICE restart: self-initiated (ICE failed, backing off)",
				"playerId", pid, "attempt", localCF, "backoff", backoff, "offerer", offerer)

			// Snapshot the current peer before sleeping.  A concurrent remote-
			// triggered restart (HandleIceMsg → go p.onRestart(msg)) may call
			// ReplaceICEPeer during the backoff, creating a new peer with the
			// offerer's credentials.  After waking we compare pointers: if the
			// peer changed, the remote restart already handled it and we abort
			// to avoid overwriting the good peer with a credential-less one.
			peerBefore := h.adapter.peerMgr.GetICEPeer(pid)

			select {
			case <-time.After(backoff):
			case <-h.adapter.ctx.Done():
				return
			}

			peerAfter := h.adapter.peerMgr.GetICEPeer(pid)
			if peerAfter == nil {
				h.logger.Info("ICE restart aborted: peer disconnected during backoff", "playerId", pid)
				return
			}
			if peerAfter != peerBefore {
				h.logger.Info("ICE restart aborted: peer was replaced by remote restart during backoff", "playerId", pid)
				return
			}
		} else if !msg.Manual {
			h.logger.Info("ICE restart: remote-triggered (new credentials received)", "playerId", pid, "attempt", localCF)
		}

		iceServers := h.adapter.GetICEServers()

		// For remote-triggered restarts, pre-load the triggering message so run()
		// can proceed immediately.  For self-initiated restarts, pass nil so run()
		// waits for the remote answerer to respond to our new credentials.
		var initialMsg *fafIceMsg
		if !selfInitiated {
			initialMsg = &msg
		}

		// Wrap onConnected to record when the connection was established.
		// The failure counter will reset on the next restart only if the
		// connection remains stable for at least 60 seconds.
		wrappedOnConnected := func() {
			*pLastConnectedAt = time.Now()
			onConnected()
		}

		// Detect a short-lived connection using the frozen snapshot.
		//
		// Timing for self-initiated restart with DisconnectedTimeout=3 s and
		// FailedTimeout=2 s:
		//   connection_duration + 3 s (disconnected) + 2 s (failed) + backoff
		//   = connection_duration + 6 s (attempt 1, 1 s backoff)
		// For a 15 s Russian ISP NAT timeout: 15 + 6 = 21 s < 45 s → shortLived ✓
		//
		// Long-running connections (e.g. 10-minute game, then dropped):
		//   snapshot ≈ 600 + 6 >> 45 → shortLived = false ✓
		// Initial failure (peer never connected): snapshot = 0 → false ✓
		// Subsequent signaling timeouts: snapshot stays frozen → shortLived persists ✓
		shortLived := localSnapshot > 0 && localSnapshot < 45*time.Second

		newPeer, err := h.adapter.peerMgr.ReplaceICEPeer(
			pid, login, offerer, iceServers, onMsg, wrappedOnConnected,
			cb,
			initialMsg,
		)
		if err != nil {
			h.logger.Error("ICE restart: failed to replace peer", "playerId", pid, "error", err)
			return
		}

		if shortLived {
			h.logger.Info("ICE restart: short-lived connection detected, switching to relay-only mode",
				"playerId", pid, "sinceConnected", localSnapshot.Round(time.Second))
			newPeer.SetRelayOnly()
		}

		// On a self-initiated restart from the answerer side, send our new
		// credentials first so the remote java-ice-adapter detects the ufrag
		// change and creates a fresh agent with new credentials of its own.
		// Without this, the answerer waits for the remote to send first, but
		// the remote has no reason to — causing an indefinite deadlock.
		if selfInitiated && !offerer {
			newPeer.SetSendFirst()
		}

		newPeer.Start(h.adapter.ctx)
	}
	return cb
}

func (h *RPCHandler) disconnectFromPeer(params []json.RawMessage) (interface{}, error) {
	if len(params) < 1 {
		return nil, fmt.Errorf("disconnectFromPeer requires 1 param (playerId)")
	}
	var playerID float64
	if err := json.Unmarshal(params[0], &playerID); err != nil {
		return nil, fmt.Errorf("disconnectFromPeer param: %w", err)
	}

	pid := int(playerID)
	h.logger.Info("disconnectFromPeer", "playerId", pid)

	// Close peer
	h.adapter.peerMgr.RemovePeer(pid)

	// Send DisconnectFromPeer to game
	h.adapter.gpgnet.SendToGame(protocol.CmdDisconnectFromPeer, []interface{}{int32(pid)})

	return nil, nil
}

func (h *RPCHandler) setLobbyInitMode(params []json.RawMessage) (interface{}, error) {
	if len(params) < 1 {
		return nil, fmt.Errorf("setLobbyInitMode requires 1 param")
	}
	var mode string
	if err := json.Unmarshal(params[0], &mode); err != nil {
		return nil, fmt.Errorf("setLobbyInitMode param: %w", err)
	}

	h.logger.Info("setLobbyInitMode", "mode", mode)

	switch mode {
	case "auto":
		h.adapter.SetLobbyInitMode(protocol.LobbyInitModeAuto)
	default:
		h.adapter.SetLobbyInitMode(protocol.LobbyInitModeNormal)
	}
	return nil, nil
}

func (h *RPCHandler) sendToGpgNet(params []json.RawMessage) (interface{}, error) {
	if len(params) < 1 {
		return nil, fmt.Errorf("sendToGpgNet requires at least 1 param (header)")
	}
	var header string
	if err := json.Unmarshal(params[0], &header); err != nil {
		return nil, fmt.Errorf("sendToGpgNet header param: %w", err)
	}

	// Convert remaining params to []interface{}
	args := make([]interface{}, 0, len(params)-1)
	for i, raw := range params[1:] {
		var v interface{}
		if err := json.Unmarshal(raw, &v); err != nil {
			return nil, fmt.Errorf("sendToGpgNet param[%d]: %w", i+1, err)
		}
		args = append(args, v)
	}

	h.logger.Debug("sendToGpgNet", "header", header, "args", args)
	return nil, h.adapter.gpgnet.SendToGame(header, args)
}

func (h *RPCHandler) status() (interface{}, error) {
	status := h.adapter.BuildStatus()
	// status() returns a JSON string (not raw object) — matches java-ice-adapter behavior
	data, err := json.Marshal(status)
	if err != nil {
		return nil, err
	}
	return string(data), nil
}

func (h *RPCHandler) quit() (interface{}, error) {
	h.logger.Info("quit requested")
	h.adapter.cancel()
	return nil, nil
}

// normalizeICEServerURL converts an ICE server URL to the opaque form required
// by pion/stun v3 (used internally by pion/ice v4 and pion/webrtc v4).
//
// Two known incompatibilities with URLs sent by FAF's icebreaker:
//
//  1. Hierarchical form ("turn://host:port"): Go's url.Parse sets Opaque=""
//     and Host="host:port". pion reads only Opaque, gets empty string → ErrHost.
//     Fix: replace "://" with ":" to produce the opaque form "turn:host:port".
//
//  2. STUN URLs with query params ("stun:host?transport=udp"): pion rejects
//     any query parameter on stun:/stuns: URLs with ErrSTUNQuery.
//     Fix: strip the query string entirely.
//     (TURN/TURNS URLs do support ?transport=tcp/udp natively in pion.)
func normalizeICEServerURL(rawURL string) string {
	// Fix 1: hierarchical form → opaque form
	// "turn://host:port" → "turn:host:port" (replace "://" with ":")
	for _, scheme := range []string{"stun://", "stuns://", "turn://", "turns://"} {
		if strings.HasPrefix(rawURL, scheme) {
			rawURL = strings.Replace(rawURL, "://", ":", 1)
			break
		}
	}
	// Fix 2: strip query string from STUN URLs (not allowed by pion)
	if (strings.HasPrefix(rawURL, "stun:") || strings.HasPrefix(rawURL, "stuns:")) {
		if idx := strings.Index(rawURL, "?"); idx >= 0 {
			rawURL = rawURL[:idx]
		}
	}
	return rawURL
}

// setIceServers stores the ICE server configuration from the FAF client.
// Format: [[{"urls":["stun:..."],"username":"...","credential":"..."}]]
func (h *RPCHandler) setIceServers(params []json.RawMessage) (interface{}, error) {
	if len(params) < 1 {
		return nil, fmt.Errorf("setIceServers requires 1 param")
	}

	// The FAF client sends an array of ICE server objects
	var servers []struct {
		URLs           []string `json:"urls"`
		Username       string   `json:"username"`
		Credential     string   `json:"credential"`
		CredentialType string   `json:"credentialType"`
	}
	if err := json.Unmarshal(params[0], &servers); err != nil {
		return nil, fmt.Errorf("setIceServers param: %w", err)
	}

	iceServers := make([]webrtc.ICEServer, 0, len(servers))
	for _, s := range servers {
		urls := make([]string, 0, len(s.URLs))
		for _, u := range s.URLs {
			normalized := normalizeICEServerURL(u)
			h.logger.Info("setIceServers url", "raw", u, "normalized", normalized)
			urls = append(urls, normalized)
		}
		is := webrtc.ICEServer{
			URLs: urls,
		}
		if s.Username != "" {
			is.Username = s.Username
		}
		if s.Credential != "" {
			is.Credential = s.Credential
		}
		iceServers = append(iceServers, is)
	}

	h.adapter.SetICEServers(iceServers)
	h.logger.Info("setIceServers", "count", len(iceServers))

	return nil, nil
}

// iceMsg handles an incoming ICE message (SDP or candidate) from the FAF client.
// Format: [remotePlayerId, msgJSON]
func (h *RPCHandler) iceMsg(params []json.RawMessage) (interface{}, error) {
	if len(params) < 2 {
		return nil, fmt.Errorf("iceMsg requires 2 params (remotePlayerId, msg)")
	}

	var remotePlayerID float64
	if err := json.Unmarshal(params[0], &remotePlayerID); err != nil {
		return nil, fmt.Errorf("iceMsg remotePlayerId param: %w", err)
	}
	pid := int(remotePlayerID)

	// The msg can be either a JSON string or a JSON object
	var msgJSON string
	if err := json.Unmarshal(params[1], &msgJSON); err != nil {
		// If it's not a string, treat the raw JSON as the message
		msgJSON = string(params[1])
	}

	// Find the ICE peer for this remote player
	icePeer := h.adapter.peerMgr.GetICEPeer(pid)
	if icePeer == nil {
		// Not an ICE peer — might be a relay peer or unknown; ignore silently
		h.logger.Debug("iceMsg for non-ICE peer, ignoring", "remotePlayerId", pid)
		return nil, nil
	}

	if err := icePeer.HandleIceMsg(msgJSON); err != nil {
		h.logger.Error("iceMsg handling failed", "remotePlayerId", pid, "error", err)
		return nil, nil // Don't return error to RPC client, just log
	}

	return nil, nil
}
