package relay

import (
	"context"
	"net"
	"testing"
	"time"

	"fafsturmudprelay/internal/protocol"
)

func setupRouterTest(t *testing.T) (*Router, *net.UDPConn, *SessionManager, *Metrics) {
	t.Helper()
	metrics := &Metrics{}
	sm := NewSessionManager(metrics)

	addr, _ := net.ResolveUDPAddr("udp", "127.0.0.1:0")
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		t.Fatalf("ListenUDP: %v", err)
	}

	return NewRouter(conn, sm, metrics, nil), conn, sm, metrics
}

func TestRouterUnicast(t *testing.T) {
	metrics := &Metrics{}
	sm := NewSessionManager(metrics)

	// Create relay server socket
	relayAddr, _ := net.ResolveUDPAddr("udp", "127.0.0.1:0")
	relayConn, err := net.ListenUDP("udp", relayAddr)
	if err != nil {
		t.Fatalf("ListenUDP: %v", err)
	}
	defer relayConn.Close()

	// Create two "player" sockets
	p1Addr, _ := net.ResolveUDPAddr("udp", "127.0.0.1:0")
	p1Conn, _ := net.ListenUDP("udp", p1Addr)
	defer p1Conn.Close()

	p2Addr, _ := net.ResolveUDPAddr("udp", "127.0.0.1:0")
	p2Conn, _ := net.ListenUDP("udp", p2Addr)
	defer p2Conn.Close()

	// Set up session with both players
	sm.CreateSession("game1", 12)
	sm.AddPlayerToSession("game1", 1, "p1")
	sm.AddPlayerToSession("game1", 2, "p2")

	// Pre-register player 2's address so router can send to it
	session := sm.GetSession("game1")
	session.UpdatePlayerAddr(2, p2Conn.LocalAddr().(*net.UDPAddr).AddrPort(), time.Now())

	// Start router
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// We need a logger for the router
	router := NewRouter(relayConn, sm, metrics, nil)
	go router.Run(ctx)

	// Player 1 sends a packet destined for Player 2
	payload := []byte("hello from p1")
	pkt, _ := protocol.EncodeRelayPacket(1, 2, payload)
	p1Conn.WriteToUDP(pkt, relayConn.LocalAddr().(*net.UDPAddr))

	// Player 2 should receive it
	p2Conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 1500)
	n, _, err := p2Conn.ReadFromUDP(buf)
	if err != nil {
		t.Fatalf("p2 ReadFromUDP: %v", err)
	}

	// The received packet includes the relay header
	received, _ := protocol.DecodeRelayPacket(buf[:n])
	if received.SenderID != 1 {
		t.Errorf("SenderID = %d, want 1", received.SenderID)
	}
	if received.DestID != 2 {
		t.Errorf("DestID = %d, want 2", received.DestID)
	}
	if string(received.Payload) != "hello from p1" {
		t.Errorf("Payload = %q, want %q", received.Payload, "hello from p1")
	}
}

func TestRouterBroadcast(t *testing.T) {
	metrics := &Metrics{}
	sm := NewSessionManager(metrics)

	relayAddr, _ := net.ResolveUDPAddr("udp", "127.0.0.1:0")
	relayConn, _ := net.ListenUDP("udp", relayAddr)
	defer relayConn.Close()

	// Create 3 player sockets
	players := make([]*net.UDPConn, 3)
	for i := range players {
		addr, _ := net.ResolveUDPAddr("udp", "127.0.0.1:0")
		conn, _ := net.ListenUDP("udp", addr)
		players[i] = conn
		defer conn.Close()
	}

	sm.CreateSession("game1", 12)
	for i := range players {
		sm.AddPlayerToSession("game1", uint32(i+1), "")
	}

	session := sm.GetSession("game1")
	for i, p := range players {
		session.UpdatePlayerAddr(uint32(i+1), p.LocalAddr().(*net.UDPAddr).AddrPort(), time.Now())
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	router := NewRouter(relayConn, sm, metrics, nil)
	go router.Run(ctx)

	// Player 1 broadcasts
	pkt, _ := protocol.EncodeRelayPacket(1, protocol.BroadcastDest, []byte("broadcast"))
	players[0].WriteToUDP(pkt, relayConn.LocalAddr().(*net.UDPAddr))

	// Players 2 and 3 should receive, player 1 should NOT
	for i := 1; i < 3; i++ {
		players[i].SetReadDeadline(time.Now().Add(2 * time.Second))
		buf := make([]byte, 1500)
		n, _, err := players[i].ReadFromUDP(buf)
		if err != nil {
			t.Fatalf("player %d ReadFromUDP: %v", i+1, err)
		}
		received, _ := protocol.DecodeRelayPacket(buf[:n])
		if string(received.Payload) != "broadcast" {
			t.Errorf("player %d got %q, want %q", i+1, received.Payload, "broadcast")
		}
	}
}

func TestRouterHelloLearnsAddress(t *testing.T) {
	metrics := &Metrics{}
	sm := NewSessionManager(metrics)

	relayAddr, _ := net.ResolveUDPAddr("udp", "127.0.0.1:0")
	relayConn, _ := net.ListenUDP("udp", relayAddr)
	defer relayConn.Close()

	// Create player sockets
	p1Addr, _ := net.ResolveUDPAddr("udp", "127.0.0.1:0")
	p1Conn, _ := net.ListenUDP("udp", p1Addr)
	defer p1Conn.Close()

	p2Addr, _ := net.ResolveUDPAddr("udp", "127.0.0.1:0")
	p2Conn, _ := net.ListenUDP("udp", p2Addr)
	defer p2Conn.Close()

	sm.CreateSession("game1", 12)
	sm.AddPlayerToSession("game1", 1, "p1")
	sm.AddPlayerToSession("game1", 2, "p2")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	router := NewRouter(relayConn, sm, metrics, nil)
	go router.Run(ctx)

	// Both players send hello packets (destID == senderID) to learn addresses
	hello1, _ := protocol.EncodeRelayPacket(1, 1, nil)
	p1Conn.WriteToUDP(hello1, relayConn.LocalAddr().(*net.UDPAddr))

	hello2, _ := protocol.EncodeRelayPacket(2, 2, nil)
	p2Conn.WriteToUDP(hello2, relayConn.LocalAddr().(*net.UDPAddr))

	// Wait for router to process
	time.Sleep(100 * time.Millisecond)

	// Verify addresses were learned (use GetPlayerAddr to avoid data race —
	// the router goroutine may still be writing to PlayerInfo fields).
	session := sm.GetSession("game1")
	learned1, ok1 := session.GetPlayerAddr(1)
	learned2, ok2 := session.GetPlayerAddr(2)
	if !ok1 || !learned1.IsValid() {
		t.Error("player 1 address not learned from hello")
	}
	if !ok2 || !learned2.IsValid() {
		t.Error("player 2 address not learned from hello")
	}

	// Hello packets should not increment dropped counter
	if metrics.PacketsDropped.Load() != 0 {
		t.Errorf("PacketsDropped = %d, want 0 (hello should not count as dropped)", metrics.PacketsDropped.Load())
	}

	// Drain echoed hello packets from p2's buffer. The router now echoes
	// hello packets back so the adapter can detect relay liveness.
	p2Conn.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	drainBuf := make([]byte, 1500)
	for {
		_, _, drainErr := p2Conn.ReadFromUDP(drainBuf)
		if drainErr != nil {
			break // timeout — no more pending packets
		}
	}

	// Now player 1 sends a real packet to player 2 — should work without pre-pinning
	payload := []byte("after hello")
	pkt, _ := protocol.EncodeRelayPacket(1, 2, payload)
	p1Conn.WriteToUDP(pkt, relayConn.LocalAddr().(*net.UDPAddr))

	p2Conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 1500)
	n, _, err := p2Conn.ReadFromUDP(buf)
	if err != nil {
		t.Fatalf("p2 ReadFromUDP: %v", err)
	}
	received, _ := protocol.DecodeRelayPacket(buf[:n])
	if string(received.Payload) != "after hello" {
		t.Errorf("Payload = %q, want %q", received.Payload, "after hello")
	}
}

func TestRouterHelloEcho(t *testing.T) {
	metrics := &Metrics{}
	sm := NewSessionManager(metrics)

	relayAddr, _ := net.ResolveUDPAddr("udp", "127.0.0.1:0")
	relayConn, _ := net.ListenUDP("udp", relayAddr)
	defer relayConn.Close()

	p1Addr, _ := net.ResolveUDPAddr("udp", "127.0.0.1:0")
	p1Conn, _ := net.ListenUDP("udp", p1Addr)
	defer p1Conn.Close()

	sm.CreateSession("game1", 12)
	sm.AddPlayerToSession("game1", 1, "p1")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	router := NewRouter(relayConn, sm, metrics, nil)
	go router.Run(ctx)

	// Player 1 sends a hello (destID == senderID)
	hello, _ := protocol.EncodeRelayPacket(1, 1, nil)
	p1Conn.WriteToUDP(hello, relayConn.LocalAddr().(*net.UDPAddr))

	// Should receive the echo back
	p1Conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 1500)
	n, _, err := p1Conn.ReadFromUDP(buf)
	if err != nil {
		t.Fatalf("expected hello echo, got error: %v", err)
	}

	received, _ := protocol.DecodeRelayPacket(buf[:n])
	if received.SenderID != 1 || received.DestID != 1 {
		t.Errorf("echo SenderID=%d DestID=%d, want 1/1", received.SenderID, received.DestID)
	}

	// Hello echo should increment PacketsRouted, not PacketsDropped
	if metrics.PacketsRouted.Load() < 1 {
		t.Error("hello echo should increment PacketsRouted")
	}
	if metrics.PacketsDropped.Load() != 0 {
		t.Errorf("PacketsDropped = %d, want 0", metrics.PacketsDropped.Load())
	}
}

func TestRouterDropsUnknownPlayer(t *testing.T) {
	metrics := &Metrics{}
	sm := NewSessionManager(metrics)

	relayAddr, _ := net.ResolveUDPAddr("udp", "127.0.0.1:0")
	relayConn, _ := net.ListenUDP("udp", relayAddr)
	defer relayConn.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	router := NewRouter(relayConn, sm, metrics, nil)
	go router.Run(ctx)

	// Send from unknown player
	clientAddr, _ := net.ResolveUDPAddr("udp", "127.0.0.1:0")
	clientConn, _ := net.ListenUDP("udp", clientAddr)
	defer clientConn.Close()

	pkt, _ := protocol.EncodeRelayPacket(9999, 1, []byte("test"))
	clientConn.WriteToUDP(pkt, relayConn.LocalAddr().(*net.UDPAddr))

	time.Sleep(100 * time.Millisecond)
	if metrics.PacketsDropped.Load() < 1 {
		t.Error("expected dropped packet for unknown player")
	}
}
