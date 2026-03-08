package adapter

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"
)

// TestPeerStartIdempotent verifies that Start() can be called multiple
// times without leaking goroutines or panicking.
// Regression: Before fix, double-Start overwrote cancel and leaked goroutines.
func TestPeerStartIdempotent(t *testing.T) {
	laddr, _ := net.ResolveUDPAddr("udp", "127.0.0.1:0")
	relayConn, _ := net.ListenUDP("udp", laddr)
	defer relayConn.Close()

	relay := &RelayConnection{conn: relayConn, relayAddr: laddr.AddrPort(), localID: 1, sendBuf: make([]byte, 1500)}
	pm := NewPeerManager(1, nil)
	pm.SetRelay(relay)

	peer, err := pm.AddPeer(42, "TestPlayer")
	if err != nil {
		t.Fatalf("AddPeer: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Call Start twice — second call must be a no-op
	peer.Start(ctx)
	peer.Start(ctx) // must NOT leak goroutines or panic

	// Verify peer is functional after double-start
	if peer.LocalPort() == 0 {
		t.Error("LocalPort should be non-zero")
	}

	// Close and verify goroutines stop
	peer.Close()
}

// TestPeerStartCloseConcurrent verifies no race between Start() and Close().
// Regression: Before fix, Close() read cancel without holding mu.
func TestPeerStartCloseConcurrent(t *testing.T) {
	laddr, _ := net.ResolveUDPAddr("udp", "127.0.0.1:0")
	relayConn, _ := net.ListenUDP("udp", laddr)
	defer relayConn.Close()

	relay := &RelayConnection{conn: relayConn, relayAddr: laddr.AddrPort(), localID: 1, sendBuf: make([]byte, 1500)}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Run Start and Close concurrently many times
	for i := 0; i < 100; i++ {
		peer, err := NewPeer(i+100, "p", 1, relay, nil)
		if err != nil {
			t.Fatalf("NewPeer: %v", err)
		}

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			peer.Start(ctx)
		}()
		go func() {
			defer wg.Done()
			time.Sleep(time.Microsecond) // tiny delay to increase interleaving
			peer.Close()
		}()
		wg.Wait()
	}
}

// TestPeerNilLogger verifies NewPeer handles nil logger gracefully.
// Regression: Before fix, nil logger caused panic on any log call.
func TestPeerNilLogger(t *testing.T) {
	laddr, _ := net.ResolveUDPAddr("udp", "127.0.0.1:0")
	relayConn, _ := net.ListenUDP("udp", laddr)
	defer relayConn.Close()

	relay := &RelayConnection{conn: relayConn, relayAddr: laddr.AddrPort(), localID: 1, sendBuf: make([]byte, 1500)}

	peer, err := NewPeer(42, "test", 1, relay, nil) // nil logger
	if err != nil {
		t.Fatalf("NewPeer with nil logger: %v", err)
	}
	defer peer.Close()

	// DeliverFromRelay with full channel triggers logger.Warn — must not panic
	for i := 0; i < 300; i++ {
		peer.DeliverFromRelay([]byte("test"))
	}
}

// TestLobbyInitModeThreadSafe verifies SetLobbyInitMode/LobbyInitMode are race-free.
// Regression: Before fix, lobbyInitMode was written directly without sync.
func TestLobbyInitModeThreadSafe(t *testing.T) {
	a := &Adapter{playerID: 1, login: "Test"}

	var wg sync.WaitGroup
	wg.Add(2)

	// Writer
	go func() {
		defer wg.Done()
		for i := 0; i < 1000; i++ {
			a.SetLobbyInitMode(i % 2)
		}
	}()

	// Reader
	go func() {
		defer wg.Done()
		for i := 0; i < 1000; i++ {
			mode := a.LobbyInitMode()
			if mode != 0 && mode != 1 {
				t.Errorf("unexpected mode: %d", mode)
			}
		}
	}()

	wg.Wait()
}
