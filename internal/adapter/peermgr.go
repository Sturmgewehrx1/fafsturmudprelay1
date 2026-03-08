package adapter

import (
	"io"
	"log/slog"
	"net"
	"sync"

	"github.com/pion/webrtc/v4"
)

// PeerManager manages the lifecycle of both relay peers and ICE peers.
type PeerManager struct {
	mu         sync.RWMutex
	peers      map[int]*Peer    // remote player ID → Relay Peer
	icePeers   map[int]*ICEPeer // remote player ID → ICE Peer
	localID    uint32
	relay      *RelayConnection
	logger     *slog.Logger
	nextOrder  int // monotonic counter — incremented each time a new peer is created
}

// NewPeerManager creates a new peer manager.
func NewPeerManager(localID uint32, logger *slog.Logger) *PeerManager {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &PeerManager{
		peers:    make(map[int]*Peer),
		icePeers: make(map[int]*ICEPeer),
		localID:  localID,
		logger:   logger,
	}
}

// SetRelay sets the relay connection for creating peers.
// Must be called before AddPeer.
func (pm *PeerManager) SetRelay(relay *RelayConnection) {
	pm.relay = relay
}

// AddPeer creates and starts a new peer.
func (pm *PeerManager) AddPeer(playerID int, login string) (*Peer, error) {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	if existing, ok := pm.peers[playerID]; ok {
		return existing, nil
	}

	peer, err := NewPeer(playerID, login, pm.localID, pm.relay, pm.logger)
	if err != nil {
		return nil, err
	}

	pm.nextOrder++
	peer.AddedOrder = pm.nextOrder
	pm.peers[playerID] = peer
	return peer, nil
}

// GetPeer returns a peer by remote player ID.
func (pm *PeerManager) GetPeer(playerID int) *Peer {
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	return pm.peers[playerID]
}

// AddICEPeer creates and starts a new ICE peer.
func (pm *PeerManager) AddICEPeer(playerID int, login string, offerer bool, iceServers []webrtc.ICEServer, onIceMsg func(string), onConnected func(), onRestart func(fafIceMsg)) (*ICEPeer, error) {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	if existing, ok := pm.icePeers[playerID]; ok {
		return existing, nil
	}

	peer, err := NewICEPeer(playerID, login, offerer, int(pm.localID), iceServers, onIceMsg, onConnected, onRestart, pm.logger, nil)
	if err != nil {
		return nil, err
	}

	pm.nextOrder++
	peer.AddedOrder = pm.nextOrder
	pm.icePeers[playerID] = peer
	return peer, nil
}

// ReplaceICEPeer tears down any existing ICE peer for playerID and creates a
// fresh one.  The old peer's local UDP socket is transferred to the new peer so
// that the game engine keeps sending to the same port without needing a new
// ConnectToPeer command.  If initialMsg is non-nil, it is delivered to the new
// peer's remote message channel immediately so the ICE handshake can proceed
// without waiting for the next iceMsg RPC call.
func (pm *PeerManager) ReplaceICEPeer(playerID int, login string, offerer bool, iceServers []webrtc.ICEServer, onIceMsg func(string), onConnected func(), onRestart func(fafIceMsg), initialMsg *fafIceMsg) (*ICEPeer, error) {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	// Steal the old peer's UDP socket and game address before closing it so the
	// game port is preserved and iceToGame can forward data immediately without
	// waiting for the game to send a packet first (which would re-learn gameAddr).
	// Also preserve AddedOrder so the peer keeps its position in the debug UI.
	var inheritedConn *net.UDPConn
	var inheritedOrder int
	var inheritedGameAddr *net.UDPAddr
	if old, ok := pm.icePeers[playerID]; ok {
		inheritedConn = old.conn
		inheritedOrder = old.AddedOrder
		old.mu.RLock()
		inheritedGameAddr = old.gameAddr
		old.mu.RUnlock()
		old.ownsConn = false // prevent Close() from closing the socket
		old.Close()          // waits for old goroutines to exit (SetReadDeadline unblock)
		delete(pm.icePeers, playerID)
	}

	peer, err := NewICEPeer(playerID, login, offerer, int(pm.localID), iceServers, onIceMsg, onConnected, onRestart, pm.logger, inheritedConn)
	if err != nil {
		if inheritedConn != nil {
			inheritedConn.Close() // we own it now; clean up on failure
		}
		return nil, err
	}

	// Preserve the original insertion order so the peer's row in the debug UI
	// does not jump to the bottom after an ICE restart.
	if inheritedOrder > 0 {
		peer.AddedOrder = inheritedOrder
	} else {
		pm.nextOrder++
		peer.AddedOrder = pm.nextOrder
	}

	// Transfer the game address so iceToGame can forward data to the game
	// immediately after ICE connects, without waiting for the game to send a
	// packet first.  Without this, early data from the remote peer is buffered
	// in earlyBuf — which is fine — but the transfer avoids an unnecessary
	// extra round-trip during the critical lobby handshake window.
	if inheritedGameAddr != nil {
		peer.gameAddr = inheritedGameAddr
	}
	pm.icePeers[playerID] = peer

	// Pre-load the triggering ICE message so run() can proceed immediately.
	if initialMsg != nil {
		peer.remoteMsg <- *initialMsg
	}
	return peer, nil
}

// GetICEPeer returns an ICE peer by remote player ID.
func (pm *PeerManager) GetICEPeer(playerID int) *ICEPeer {
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	return pm.icePeers[playerID]
}

// GetAllICEPeers returns all ICE peers.
func (pm *PeerManager) GetAllICEPeers() []*ICEPeer {
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	peers := make([]*ICEPeer, 0, len(pm.icePeers))
	for _, p := range pm.icePeers {
		peers = append(peers, p)
	}
	return peers
}

// RemovePeer stops and removes a peer (relay or ICE).
func (pm *PeerManager) RemovePeer(playerID int) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	if peer, ok := pm.peers[playerID]; ok {
		peer.Close()
		delete(pm.peers, playerID)
	}
	if peer, ok := pm.icePeers[playerID]; ok {
		peer.Close()
		delete(pm.icePeers, playerID)
	}
}

// GetAllPeerIDs returns all peer IDs.
func (pm *PeerManager) GetAllPeerIDs() []int {
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	ids := make([]int, 0, len(pm.peers))
	for id := range pm.peers {
		ids = append(ids, id)
	}
	return ids
}

// GetAllPeers returns all peers.
func (pm *PeerManager) GetAllPeers() []*Peer {
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	peers := make([]*Peer, 0, len(pm.peers))
	for _, p := range pm.peers {
		peers = append(peers, p)
	}
	return peers
}

// CloseAll stops and removes all peers (relay and ICE).
func (pm *PeerManager) CloseAll() {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	for id, peer := range pm.peers {
		peer.Close()
		delete(pm.peers, id)
	}
	for id, peer := range pm.icePeers {
		peer.Close()
		delete(pm.icePeers, id)
	}
}
