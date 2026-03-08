package adapter

import (
	"sort"
	"time"
)

// StatusResponse matches the exact JSON structure of java-ice-adapter's IceStatus.
// Field names use underscore_case and the "gpgpnet" typo is intentional —
// it matches the Java field name exactly.
type StatusResponse struct {
	Version        string         `json:"version"`
	IceServersSize int            `json:"ice_servers_size"`
	LobbyPort      int            `json:"lobby_port"`
	InitMode       string         `json:"init_mode"`
	Options        StatusOptions  `json:"options"`
	GPGPNet        StatusGPGPNet  `json:"gpgpnet"` // intentional typo from Java
	Relays         []StatusRelay  `json:"relays"`
	RelayConnected bool           `json:"relay_connected"` // relay server UDP reachability
}

type StatusOptions struct {
	PlayerID    int    `json:"player_id"`
	PlayerLogin string `json:"player_login"`
	RPCPort     int    `json:"rpc_port"`
	GPGNetPort  int    `json:"gpgnet_port"`
}

type StatusGPGPNet struct {
	LocalPort  int    `json:"local_port"`
	Connected  bool   `json:"connected"`
	GameState  string `json:"game_state"`
	TaskString string `json:"task_string"`
}

type StatusRelay struct {
	RemotePlayerID    int            `json:"remote_player_id"`
	RemotePlayerLogin string         `json:"remote_player_login"`
	LocalGameUDPPort  int            `json:"local_game_udp_port"`
	ICE               StatusRelayICE `json:"ice"`
	addedOrder        int            // unexported: for stable debug-UI ordering only, not marshalled to JSON
}

type StatusRelayICE struct {
	Offerer          bool    `json:"offerer"`
	State            string  `json:"state"`
	GatheringState   string  `json:"gathering_state"`
	DatachannelState string  `json:"datachannel_state"`
	Connected        bool    `json:"connected"`
	ConnectedMs      int64   `json:"connected_ms"`  // milliseconds since ICE entered Connected state; 0 if not connected
	LocCandAddr      string  `json:"loc_cand_addr"`
	RemCandAddr      string  `json:"rem_cand_addr"`
	LocCandType      string  `json:"loc_cand_type"`
	RemCandType      string  `json:"rem_cand_type"`
	TimeToConnected  float64 `json:"time_to_connected"`
	RttMs            int64   `json:"rtt_ms"`        // round-trip time in ms; 0 = not measured
	RttEstimated     bool    `json:"rtt_estimated"` // true if RTT is NTP-based estimate, not direct
	Dedicated        bool    `json:"dedicated"`     // true = connection via our dedicated relay server (not TURN/P2P)
	Transport        string  `json:"transport"`     // "udp" or "tcp"; for dedicated relay this reflects the current transport and can change at runtime
}

// BuildStatus creates a StatusResponse reflecting current adapter state.
func (a *Adapter) BuildStatus() StatusResponse {
	initMode := "normal"
	if a.LobbyInitMode() == 1 {
		initMode = "auto"
	}

	gameState := "None"
	connected := false
	gpgnetPort := 0
	if a.gpgnet != nil {
		gameState = a.gpgnet.GameState()
		connected = a.gpgnet.IsConnected()
		gpgnetPort = a.gpgnet.Port()
	}

	relays := make([]StatusRelay, 0)
	if a.peerMgr != nil {
		// Relay peers (via dedicated relay server).
		// All dedicated relay peers share the same RelayConnection, so they
		// share the same transport (UDP or TCP). The transport can change at
		// runtime (UDP → TCP fallback, TCP reconnection).
		relayTransport := "udp"
		if a.relayConn != nil {
			relayTransport = a.relayConn.Transport()
		}
		for _, peer := range a.peerMgr.GetAllPeers() {
			relays = append(relays, StatusRelay{
				RemotePlayerID:    peer.PlayerID,
				RemotePlayerLogin: peer.Login,
				LocalGameUDPPort:  peer.LocalPort(),
				addedOrder:        peer.AddedOrder,
				ICE: StatusRelayICE{
					Offerer:         false,
					State:           "Connected",
					Connected:       true,
					LocCandType:     "relay",
					RemCandType:     "relay",
					TimeToConnected: 0.0,
					Dedicated:       true,
					RttMs:           peer.RttMs(),
					RttEstimated:    true,
					Transport:       relayTransport,
				},
			})
		}
		// ICE peers (P2P via pion/ice).
		// Transport is usually UDP but can be TCP when the selected candidate
		// is a TURN relay using TCP or TLS (TURNS) to the TURN server.
		for _, peer := range a.peerMgr.GetAllICEPeers() {
			iceState := peer.ICEState()
			iceConnected := iceState == "Connected"
			locType, remType := peer.SelectedCandTypes()
			if locType == "" {
				locType = "unknown"
			}
			if remType == "" {
				remType = "unknown"
			}
			var connectedMs int64
			if connAt := peer.ConnectedAt(); iceConnected && !connAt.IsZero() {
				connectedMs = time.Since(connAt).Milliseconds()
			}
			relays = append(relays, StatusRelay{
				RemotePlayerID:    peer.PlayerID,
				RemotePlayerLogin: peer.Login,
				LocalGameUDPPort:  peer.LocalPort(),
				addedOrder:        peer.AddedOrder,
				ICE: StatusRelayICE{
					Offerer:      peer.Offerer,
					State:        iceState,
					Connected:    iceConnected,
					ConnectedMs:  connectedMs,
					LocCandType:  locType,
					RemCandType:  remType,
					RttMs:        peer.RttMs(),
					RttEstimated: true,
					Transport:    peer.SelectedTransport(),
				},
			})
		}
	}

	// Sort peers by insertion order so the debug UI shows a stable list.
	// Relay-mode peers and ICE-mode peers share the same counter in PeerManager.
	sort.Slice(relays, func(i, j int) bool {
		return relays[i].addedOrder < relays[j].addedOrder
	})

	relayConnected := false
	if a.relayConn != nil {
		relayConnected = a.relayConn.IsHealthy()
	}

	iceServersSize := len(a.GetICEServers())

	return StatusResponse{
		Version:        "faf-udp-relay-0.1.0",
		IceServersSize: iceServersSize,
		LobbyPort:      a.lobbyPort,
		InitMode:       initMode,
		Options: StatusOptions{
			PlayerID:    a.playerID,
			PlayerLogin: a.login,
			RPCPort:     a.rpcPort,
			GPGNetPort:  gpgnetPort,
		},
		GPGPNet: StatusGPGPNet{
			LocalPort:  gpgnetPort,
			Connected:  connected,
			GameState:  gameState,
			TaskString: "-",
		},
		Relays:         relays,
		RelayConnected: relayConnected,
	}
}
