package adapter

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"fafsturmudprelay/internal/protocol"
)

// getFreeTCPPort finds a free TCP port.
func getFreeTCPPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("getFreeTCPPort: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	return port
}

// waitForTCP waits for a TCP port to become available.
func waitForTCP(t *testing.T, addr string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 50*time.Millisecond)
		if err == nil {
			conn.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for TCP port %s", addr)
}

func TestPeerManagerAddRemove(t *testing.T) {
	pm := NewPeerManager(1, nil)

	// Create a dummy relay connection for the test
	laddr, _ := net.ResolveUDPAddr("udp", "127.0.0.1:0")
	conn, _ := net.ListenUDP("udp", laddr)
	defer conn.Close()

	pm.SetRelay(&RelayConnection{
		conn:      conn,
		relayAddr: laddr.AddrPort(),
		localID:   1,
		sendBuf:   make([]byte, 1500),
	})

	peer, err := pm.AddPeer(42, "TestPlayer")
	if err != nil {
		t.Fatalf("AddPeer: %v", err)
	}
	defer peer.Close()

	if peer.PlayerID != 42 {
		t.Errorf("PlayerID = %d, want 42", peer.PlayerID)
	}
	if peer.Login != "TestPlayer" {
		t.Errorf("Login = %q, want %q", peer.Login, "TestPlayer")
	}
	if peer.LocalPort() == 0 {
		t.Error("LocalPort should be non-zero")
	}

	// Add same peer again — should return existing
	peer2, err := pm.AddPeer(42, "TestPlayer")
	if err != nil {
		t.Fatalf("AddPeer duplicate: %v", err)
	}
	if peer2 != peer {
		t.Error("AddPeer should return existing peer")
	}

	// Get peer
	if pm.GetPeer(42) != peer {
		t.Error("GetPeer(42) should return the peer")
	}
	if pm.GetPeer(999) != nil {
		t.Error("GetPeer(999) should be nil")
	}

	// Remove
	pm.RemovePeer(42)
	if pm.GetPeer(42) != nil {
		t.Error("GetPeer(42) should be nil after removal")
	}
}

func TestGPGNetServerAcceptsConnection(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Create minimal adapter for GPGNet server
	a := &Adapter{
		ctx:           ctx,
		cancel:        cancel,
		playerID:      1,
		login:         "Test",
		lobbyPort:     12345,
		lobbyInitMode: protocol.LobbyInitModeNormal,
	}

	gpgnet, err := NewGPGNetServer(0, a, nil)
	if err != nil {
		t.Fatalf("NewGPGNetServer: %v", err)
	}
	defer gpgnet.Close()

	go gpgnet.Run(ctx)

	// Connect as game
	addr := fmt.Sprintf("127.0.0.1:%d", gpgnet.Port())
	waitForTCP(t, addr, 2*time.Second)

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()

	// Send GameState "Idle" — should trigger CreateLobby response
	writer := protocol.NewGPGNetWriter(conn)
	if err := writer.WriteMessage(protocol.CmdGameState, []interface{}{"Idle"}); err != nil {
		t.Fatalf("WriteMessage: %v", err)
	}

	// Read CreateLobby response
	reader := protocol.NewGPGNetReader(conn)
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	header, chunks, err := reader.ReadMessage()
	if err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	if header != protocol.CmdCreateLobby {
		t.Errorf("header = %q, want %q", header, protocol.CmdCreateLobby)
	}
	if len(chunks) != 5 {
		t.Fatalf("len(chunks) = %d, want 5", len(chunks))
	}
	// Check lobby init mode
	if v, ok := chunks[0].(int32); !ok || v != 0 {
		t.Errorf("lobbyInitMode = %v, want 0", chunks[0])
	}
	// Check player name
	if v, ok := chunks[2].(string); !ok || v != "Test" {
		t.Errorf("login = %v, want %q", chunks[2], "Test")
	}
}

func TestRPCServerHandlesRequests(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	a := &Adapter{
		ctx:      ctx,
		cancel:   cancel,
		playerID: 1,
		login:    "Test",
	}

	// Create GPGNet server for the adapter
	a.gpgnet, _ = NewGPGNetServer(0, a, nil)
	defer a.gpgnet.Close()

	// Need a peer manager
	a.peerMgr = NewPeerManager(1, nil)
	laddr, _ := net.ResolveUDPAddr("udp", "127.0.0.1:0")
	conn, _ := net.ListenUDP("udp", laddr)
	defer conn.Close()
	a.peerMgr.SetRelay(&RelayConnection{conn: conn, relayAddr: laddr.AddrPort(), localID: 1, sendBuf: make([]byte, 1500)})

	handler := NewRPCHandler(a, nil)
	rpc, err := NewRPCServer(0, handler, nil)
	if err != nil {
		t.Fatalf("NewRPCServer: %v", err)
	}
	defer rpc.Close()
	a.rpc = rpc

	go rpc.Run(ctx)

	// Connect as FAF client
	addr := fmt.Sprintf("127.0.0.1:%d", rpc.Port())
	waitForTCP(t, addr, 2*time.Second)

	rpcConn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer rpcConn.Close()

	rpcWriter := protocol.NewJSONRPCWriter(rpcConn)
	rpcReader := protocol.NewJSONRPCReader(rpcConn)

	// Call status()
	statusReq := map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "status",
		"params":  []interface{}{},
	}
	data, _ := json.Marshal(statusReq)
	rpcConn.Write(data)
	rpcConn.Write([]byte("\n"))

	rpcConn.SetReadDeadline(time.Now().Add(2 * time.Second))
	resp, err := rpcReader.ReadRaw()
	if err != nil {
		t.Fatalf("ReadRaw: %v", err)
	}

	// Parse response
	var rpcResp protocol.JSONRPCResponse
	if err := json.Unmarshal(resp, &rpcResp); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	// Result should be a JSON string containing status
	statusStr, ok := rpcResp.Result.(string)
	if !ok {
		t.Fatalf("result type = %T, want string", rpcResp.Result)
	}

	// Parse the status JSON
	var status StatusResponse
	if err := json.Unmarshal([]byte(statusStr), &status); err != nil {
		t.Fatalf("Unmarshal status: %v", err)
	}

	// Verify the "gpgpnet" typo is present in the JSON
	if !strings.Contains(statusStr, `"gpgpnet"`) {
		t.Error("status JSON should contain 'gpgpnet' (with typo)")
	}

	if status.Options.PlayerID != 1 {
		t.Errorf("PlayerID = %d, want 1", status.Options.PlayerID)
	}
	if status.Options.PlayerLogin != "Test" {
		t.Errorf("PlayerLogin = %q, want %q", status.Options.PlayerLogin, "Test")
	}

	// Test setLobbyInitMode
	_ = rpcWriter.WriteNotification("test", nil) // just to ensure writer works
}

func TestStatusGpgpnetTypo(t *testing.T) {
	a := &Adapter{
		playerID: 42,
		login:    "TestPlayer",
		rpcPort:  7236,
	}

	status := a.BuildStatus()
	data, err := json.Marshal(status)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	jsonStr := string(data)
	if !strings.Contains(jsonStr, `"gpgpnet"`) {
		t.Error("JSON should contain 'gpgpnet' (double p typo)")
	}
	if strings.Contains(jsonStr, `"gpgnet"`) && !strings.Contains(jsonStr, `"gpgpnet"`) {
		t.Error("JSON has 'gpgnet' without the typo — should be 'gpgpnet'")
	}
}
