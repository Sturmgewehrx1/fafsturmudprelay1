package adapter

import (
	"context"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"fafsturmudprelay/internal/protocol"
)

// TestGPGNetReconnectRace verifies that a new game connection does not
// get its state wiped by the old connection's cleanup goroutine.
// Regression: Old handleConnection would nil out conn/writer/connected
// even after a new connection had taken over.
func TestGPGNetReconnectRace(t *testing.T) {
	a := &Adapter{
		playerID: 1,
		login:    "Test",
		rpcPort:  0,
	}

	gpg, err := NewGPGNetServer(0, a, nil)
	if err != nil {
		t.Fatalf("NewGPGNetServer: %v", err)
	}
	defer gpg.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go gpg.Run(ctx)
	time.Sleep(50 * time.Millisecond) // let server start

	port := gpg.Port()

	// Connect first game
	conn1, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatalf("dial 1: %v", err)
	}
	time.Sleep(50 * time.Millisecond)

	if !gpg.IsConnected() {
		t.Fatal("game should be connected after first dial")
	}

	// Connect second game (triggers close of first)
	conn2, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatalf("dial 2: %v", err)
	}
	time.Sleep(100 * time.Millisecond)

	// The server should still be connected (to conn2)
	if !gpg.IsConnected() {
		t.Fatal("game should still be connected after second game connects — old goroutine wiped state")
	}

	conn1.Close()
	conn2.Close()
}

// TestGPGNetStateResetOnReconnect verifies that lobby state (lobbyOnce,
// lobbyReady, gameState, messageQueue) is properly reset when a new
// game connects.
func TestGPGNetStateResetOnReconnect(t *testing.T) {
	a := &Adapter{
		playerID: 1,
		login:    "Test",
		rpcPort:  0,
	}

	gpg, err := NewGPGNetServer(0, a, nil)
	if err != nil {
		t.Fatalf("NewGPGNetServer: %v", err)
	}
	defer gpg.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go gpg.Run(ctx)
	time.Sleep(50 * time.Millisecond)

	port := gpg.Port()

	// First connection: simulate game sending GameState Idle then Lobby
	conn1, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatalf("dial 1: %v", err)
	}
	time.Sleep(50 * time.Millisecond)

	// Send GameState "Idle" via GPGNet protocol
	w := protocol.NewGPGNetWriter(conn1)
	w.WriteMessage(protocol.CmdGameState, []interface{}{"Idle"})
	time.Sleep(50 * time.Millisecond)

	if gpg.GameState() != protocol.GameStateIdle {
		t.Fatalf("expected Idle, got %s", gpg.GameState())
	}

	// Disconnect first game
	conn1.Close()
	time.Sleep(100 * time.Millisecond)

	// Second connection: state should be reset to None
	conn2, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatalf("dial 2: %v", err)
	}
	defer conn2.Close()
	time.Sleep(50 * time.Millisecond)

	if gpg.GameState() != protocol.GameStateNone {
		t.Errorf("expected None after reconnect, got %s", gpg.GameState())
	}
}

// TestGPGNetQueueResetOnReconnect verifies that the message queue
// and lobbyOnce are reset, allowing a new game to go through the
// full Idle→Lobby flow again.
func TestGPGNetQueueResetOnReconnect(t *testing.T) {
	a := &Adapter{
		playerID: 1,
		login:    "Test",
		rpcPort:  0,
	}

	gpg, err := NewGPGNetServer(0, a, nil)
	if err != nil {
		t.Fatalf("NewGPGNetServer: %v", err)
	}
	defer gpg.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go gpg.Run(ctx)
	time.Sleep(50 * time.Millisecond)

	port := gpg.Port()

	// Queue a message before any game connects
	gpg.SendToGameQueued("HostGame", []interface{}{"scmp_001"})

	// First game: advance to Lobby which flushes the queue
	conn1, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatalf("dial 1: %v", err)
	}
	time.Sleep(50 * time.Millisecond)

	w := protocol.NewGPGNetWriter(conn1)
	w.WriteMessage(protocol.CmdGameState, []interface{}{"Idle"})
	time.Sleep(50 * time.Millisecond)
	w.WriteMessage(protocol.CmdGameState, []interface{}{"Lobby"})
	time.Sleep(50 * time.Millisecond)

	// Disconnect
	conn1.Close()
	time.Sleep(100 * time.Millisecond)

	// Second game connects. Queue should be empty and lobbyOnce reset.
	conn2, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatalf("dial 2: %v", err)
	}
	defer conn2.Close()
	time.Sleep(50 * time.Millisecond)

	// Queue a new message for the second game
	gpg.SendToGameQueued("JoinGame", []interface{}{"127.0.0.1:12345", "Test", int32(2)})

	// The message should be queued (lobbyReady is a fresh channel)
	gpg.queueMu.Lock()
	qLen := len(gpg.messageQueue)
	gpg.queueMu.Unlock()

	if qLen != 1 {
		t.Errorf("expected 1 queued message for new game, got %d", qLen)
	}
}

// TestUDPErrorBackoffExists verifies the udpErrorBackoff constant is set
// to a reasonable value to prevent tight error loops.
func TestUDPErrorBackoffExists(t *testing.T) {
	if udpErrorBackoff < 10*time.Millisecond {
		t.Errorf("udpErrorBackoff too small: %v", udpErrorBackoff)
	}
	if udpErrorBackoff > 5*time.Second {
		t.Errorf("udpErrorBackoff too large: %v", udpErrorBackoff)
	}
}

// TestPeerCloseOnBrokenSocket verifies that Peer.Close() properly stops
// goroutines even after the socket has been closed externally.
func TestPeerCloseOnBrokenSocket(t *testing.T) {
	laddr, _ := net.ResolveUDPAddr("udp", "127.0.0.1:0")
	relayConn, _ := net.ListenUDP("udp", laddr)
	defer relayConn.Close()

	relay := &RelayConnection{conn: relayConn, relayAddr: laddr.AddrPort(), localID: 1, sendBuf: make([]byte, 1500)}

	peer, err := NewPeer(42, "test", 1, relay, nil)
	if err != nil {
		t.Fatalf("NewPeer: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	peer.Start(ctx)

	// Close the peer's socket externally (simulates OS-level socket close)
	peer.conn.Close()
	time.Sleep(200 * time.Millisecond) // wait for backoff to trigger

	// Cancel should still work and not hang
	done := make(chan struct{})
	go func() {
		cancel()
		peer.Close()
		close(done)
	}()

	select {
	case <-done:
		// Good
	case <-time.After(2 * time.Second):
		t.Fatal("Close() hung after socket was externally broken")
	}
}

// TestConcurrentDeliverFromRelay verifies that high-volume packet delivery
// to a peer with a full channel drops gracefully without blocking.
func TestConcurrentDeliverFromRelay(t *testing.T) {
	laddr, _ := net.ResolveUDPAddr("udp", "127.0.0.1:0")
	relayConn, _ := net.ListenUDP("udp", laddr)
	defer relayConn.Close()

	relay := &RelayConnection{conn: relayConn, relayAddr: laddr.AddrPort(), localID: 1, sendBuf: make([]byte, 1500)}

	peer, err := NewPeer(42, "test", 1, relay, nil)
	if err != nil {
		t.Fatalf("NewPeer: %v", err)
	}
	defer peer.Close()

	// Don't start the peer (so nothing drains the channel)
	// Fire 1000 deliveries concurrently — must not block or panic
	var wg sync.WaitGroup
	for i := 0; i < 1000; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			peer.DeliverFromRelay([]byte("test-payload"))
		}()
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		// Good — none blocked
	case <-time.After(5 * time.Second):
		t.Fatal("DeliverFromRelay blocked with full channel")
	}
}
