package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"fafsturmudprelay/internal/protocol"
	"fafsturmudprelay/internal/relay"
	"fafsturmudprelay/internal/testutil"
)

// simPlayer represents one simulated player with its own UDP socket.
type simPlayer struct {
	playerID  uint32
	sessionID string
	conn      *net.UDPConn
	relayAddr *net.UDPAddr
	sendBuf   []byte // pre-allocated send buffer

	// stats
	pktsSent atomic.Uint64
	pktsRecv atomic.Uint64
	bytesSent atomic.Uint64
	bytesRecv atomic.Uint64
}

// globalStats are accumulated across all players.
type globalStats struct {
	totalPktsSent  atomic.Uint64
	totalPktsRecv  atomic.Uint64
	totalBytesSent atomic.Uint64
	totalBytesRecv atomic.Uint64
}

func main() {
	sessions := flag.Int("sessions", 50, "Number of concurrent game sessions")
	players := flag.Int("players", 12, "Players per session")
	pps := flag.Int("pps", 30, "Broadcast packets per second per player")
	payloadSize := flag.Int("payload", 200, "Payload size in bytes (max 1392)")
	duration := flag.Duration("duration", 60*time.Second, "Test duration")
	rampUp := flag.Duration("ramp-up", 10*time.Second, "Time to ramp up all sessions")
	externalServer := flag.String("server", "", "External relay server address (host:port). If empty, starts embedded server")
	externalHTTP := flag.String("http", "", "External relay HTTP API (http://host:port). Required if --server is set")
	apiKey := flag.String("api-key", "faf-relay-s3cret-42", "API key for relay server HTTP endpoints")
	flag.Parse()

	if *payloadSize > protocol.MaxRelayPayload {
		log.Fatalf("Payload size %d exceeds max %d", *payloadSize, protocol.MaxRelayPayload)
	}

	totalPlayers := *sessions * *players
	log.Printf("=== FAF Relay Stress Test ===")
	log.Printf("Sessions: %d | Players/Session: %d | Total Players: %d", *sessions, *players, totalPlayers)
	log.Printf("PPS/Player: %d | Payload: %d bytes | Duration: %v | Ramp-up: %v", *pps, *payloadSize, *duration, *rampUp)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Handle Ctrl+C
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)
	go func() {
		<-sigCh
		log.Println("Interrupted — shutting down...")
		cancel()
	}()

	var relayUDPAddr string
	var relayHTTPAddr string

	if *externalServer != "" {
		// Use external relay server
		relayUDPAddr = *externalServer
		relayHTTPAddr = *externalHTTP
		if relayHTTPAddr == "" {
			log.Fatal("--http is required when using --server")
		}
		log.Printf("Using external relay: UDP=%s HTTP=%s", relayUDPAddr, relayHTTPAddr)
	} else {
		// Start embedded relay server
		udpPort, err := testutil.GetFreeUDPPort()
		if err != nil {
			log.Fatalf("Cannot get free UDP port: %v", err)
		}
		httpPort, err := testutil.GetFreeTCPPort()
		if err != nil {
			log.Fatalf("Cannot get free TCP port: %v", err)
		}

		srv := relay.NewServer(relay.ServerConfig{
			UDPAddr:         fmt.Sprintf(":%d", udpPort),
			HTTPAddr:        fmt.Sprintf(":%d", httpPort),
			RateLimitGlobal: 100000,
			RateLimitPerIP:  100000,
		})
		go srv.Run(ctx)
		time.Sleep(300 * time.Millisecond)

		relayUDPAddr = fmt.Sprintf("127.0.0.1:%d", udpPort)
		relayHTTPAddr = fmt.Sprintf("http://127.0.0.1:%d", httpPort)
		log.Printf("Embedded relay started: UDP=%s HTTP=%s", relayUDPAddr, relayHTTPAddr)
	}

	resolvedAddr, err := net.ResolveUDPAddr("udp", relayUDPAddr)
	if err != nil {
		log.Fatalf("Cannot resolve relay UDP address: %v", err)
	}

	// Create payload template (random bytes to simulate game data)
	payloadTemplate := make([]byte, *payloadSize)
	rand.Read(payloadTemplate)

	var gstats globalStats
	var allPlayers []*simPlayer

	// Ramp-up: create sessions incrementally
	rampDelay := *rampUp / time.Duration(*sessions)
	if rampDelay < time.Millisecond {
		rampDelay = time.Millisecond
	}

	log.Printf("Ramping up %d sessions (interval: %v)...", *sessions, rampDelay)

	var wgSenders sync.WaitGroup
	var wgReceivers sync.WaitGroup

	startTime := time.Now()

	// Player ID counter (globally unique across all sessions)
	nextPlayerID := uint32(1)

	for s := 0; s < *sessions; s++ {
		if ctx.Err() != nil {
			break
		}

		sessionID := fmt.Sprintf("stress-%d", s+1)

		// Create session via HTTP
		if err := httpCreateSession(relayHTTPAddr, sessionID, *players, *apiKey); err != nil {
			log.Printf("WARN: Failed to create session %s: %v", sessionID, err)
			continue
		}

		// Create players for this session
		sessionPlayers := make([]*simPlayer, 0, *players)
		for p := 0; p < *players; p++ {
			pid := nextPlayerID
			nextPlayerID++

			// Join session via HTTP
			login := fmt.Sprintf("Bot%d", pid)
			if err := httpJoinSession(relayHTTPAddr, sessionID, pid, login, *apiKey); err != nil {
				log.Printf("WARN: Failed to join player %d to session %s: %v", pid, sessionID, err)
				continue
			}

			// Open UDP socket
			udpConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
			if err != nil {
				log.Printf("WARN: Failed to open UDP socket for player %d: %v", pid, err)
				continue
			}

			sp := &simPlayer{
				playerID:  pid,
				sessionID: sessionID,
				conn:      udpConn,
				relayAddr: resolvedAddr,
				sendBuf:   make([]byte, protocol.MaxRelayPacket),
			}
			sessionPlayers = append(sessionPlayers, sp)
			allPlayers = append(allPlayers, sp)

			// Send hello packet (dest=self) to register our address with the relay
			hello, _ := protocol.EncodeRelayPacket(pid, pid, nil)
			udpConn.WriteToUDP(hello, resolvedAddr)
		}

		// Start sender and receiver goroutines for each player
		for _, sp := range sessionPlayers {
			sp := sp // capture

			// Receiver goroutine
			wgReceivers.Add(1)
			go func() {
				defer wgReceivers.Done()
				recvLoop(ctx, sp, &gstats)
			}()

			// Sender goroutine
			wgSenders.Add(1)
			go func() {
				defer wgSenders.Done()
				sendLoop(ctx, sp, *pps, payloadTemplate, &gstats)
			}()
		}

		if (s+1)%10 == 0 || s+1 == *sessions {
			log.Printf("  Sessions created: %d/%d (%d players active)", s+1, *sessions, len(allPlayers))
		}

		if s+1 < *sessions {
			time.Sleep(rampDelay)
		}
	}

	rampDone := time.Since(startTime)
	log.Printf("Ramp-up complete in %v. %d players active. Running for %v...", rampDone.Round(time.Millisecond), len(allPlayers), *duration)

	// Stats reporter goroutine
	go statsReporter(ctx, &gstats, relayHTTPAddr, startTime)

	// Wait for duration
	select {
	case <-ctx.Done():
	case <-time.After(*duration):
		log.Println("Duration reached — shutting down...")
		cancel()
	}

	// Wait for goroutines
	wgSenders.Wait()

	// Close all UDP sockets to unblock receivers
	for _, sp := range allPlayers {
		sp.conn.Close()
	}
	wgReceivers.Wait()

	// Final summary
	printSummary(&gstats, allPlayers, startTime, relayHTTPAddr)
}

// sendLoop sends broadcast packets at the configured rate.
func sendLoop(ctx context.Context, sp *simPlayer, pps int, payload []byte, gs *globalStats) {
	interval := time.Second / time.Duration(pps)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Pre-encode header into send buffer
	protocol.EncodeRelayHeader(sp.sendBuf, sp.playerID, protocol.BroadcastDest)
	copy(sp.sendBuf[protocol.RelayHeaderSize:], payload)
	pktSize := protocol.RelayHeaderSize + len(payload)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_, err := sp.conn.WriteToUDP(sp.sendBuf[:pktSize], sp.relayAddr)
			if err != nil {
				// socket probably closed
				return
			}
			sp.pktsSent.Add(1)
			sp.bytesSent.Add(uint64(pktSize))
			gs.totalPktsSent.Add(1)
			gs.totalBytesSent.Add(uint64(pktSize))
		}
	}
}

// recvLoop reads packets from the relay and counts them.
func recvLoop(ctx context.Context, sp *simPlayer, gs *globalStats) {
	buf := make([]byte, 1500)
	for {
		sp.conn.SetReadDeadline(time.Now().Add(1 * time.Second))
		n, _, err := sp.conn.ReadFromUDP(buf)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			// Timeout or closed — just loop
			continue
		}
		sp.pktsRecv.Add(1)
		sp.bytesRecv.Add(uint64(n))
		gs.totalPktsRecv.Add(1)
		gs.totalBytesRecv.Add(uint64(n))
	}
}

// statsReporter prints live stats every second.
func statsReporter(ctx context.Context, gs *globalStats, httpAddr string, startTime time.Time) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	var lastSent, lastRecv, lastBytesSent, lastBytesRecv uint64

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sent := gs.totalPktsSent.Load()
			recv := gs.totalPktsRecv.Load()
			bSent := gs.totalBytesSent.Load()
			bRecv := gs.totalBytesRecv.Load()

			dpSent := sent - lastSent
			dpRecv := recv - lastRecv
			dbSent := bSent - lastBytesSent
			dbRecv := bRecv - lastBytesRecv

			lastSent = sent
			lastRecv = recv
			lastBytesSent = bSent
			lastBytesRecv = bRecv

			elapsed := time.Since(startTime).Round(time.Second)

			// Get server metrics
			serverInfo := ""
			health := fetchHealth(httpAddr)
			if health != nil {
				serverInfo = fmt.Sprintf(" | Server: sessions=%d players=%d",
					health.ActiveSessions, health.ActivePlayers)
			}

			// Memory stats
			var m runtime.MemStats
			runtime.ReadMemStats(&m)

			log.Printf("[%v] TX: %d pkt/2s (%.0f pps) %.2f MB/2s | RX: %d pkt/2s (%.0f pps) %.2f MB/2s | Mem: %.1f MB%s",
				elapsed,
				dpSent, float64(dpSent)/2.0, float64(dbSent)/1024/1024,
				dpRecv, float64(dpRecv)/2.0, float64(dbRecv)/1024/1024,
				float64(m.Alloc)/1024/1024,
				serverInfo,
			)
		}
	}
}

// printSummary outputs final test results.
func printSummary(gs *globalStats, players []*simPlayer, startTime time.Time, httpAddr string) {
	elapsed := time.Since(startTime)

	totalSent := gs.totalPktsSent.Load()
	totalRecv := gs.totalPktsRecv.Load()
	totalBytesSent := gs.totalBytesSent.Load()
	totalBytesRecv := gs.totalBytesRecv.Load()

	log.Println("")
	log.Println("╔══════════════════════════════════════════════════════════════╗")
	log.Println("║                   STRESSTEST ERGEBNIS                       ║")
	log.Println("╠══════════════════════════════════════════════════════════════╣")
	log.Printf("║  Dauer:              %-39v ║", elapsed.Round(time.Millisecond))
	log.Printf("║  Spieler:            %-39d ║", len(players))
	log.Println("╠══════════════════════════════════════════════════════════════╣")
	log.Printf("║  Pakete gesendet:    %-39s ║", formatNumber(totalSent))
	log.Printf("║  Pakete empfangen:   %-39s ║", formatNumber(totalRecv))
	log.Printf("║  Bytes gesendet:     %-39s ║", formatBytes(totalBytesSent))
	log.Printf("║  Bytes empfangen:    %-39s ║", formatBytes(totalBytesRecv))
	log.Println("╠══════════════════════════════════════════════════════════════╣")

	secs := elapsed.Seconds()
	if secs > 0 {
		log.Printf("║  Avg TX:             %-39s ║", fmt.Sprintf("%.0f pps / %.2f MB/s", float64(totalSent)/secs, float64(totalBytesSent)/secs/1024/1024))
		log.Printf("║  Avg RX:             %-39s ║", fmt.Sprintf("%.0f pps / %.2f MB/s", float64(totalRecv)/secs, float64(totalBytesRecv)/secs/1024/1024))
	}

	// Memory
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	log.Printf("║  Speicher (Alloc):   %-39s ║", fmt.Sprintf("%.1f MB", float64(m.Alloc)/1024/1024))
	log.Printf("║  Speicher (Sys):     %-39s ║", fmt.Sprintf("%.1f MB", float64(m.Sys)/1024/1024))
	log.Printf("║  Goroutines:         %-39d ║", runtime.NumGoroutine())

	// Server health
	health := fetchHealth(httpAddr)
	if health != nil {
		log.Println("╠══════════════════════════════════════════════════════════════╣")
		log.Println("║  SERVER                                                     ║")
		log.Printf("║  Sessions aktiv:     %-39d ║", health.ActiveSessions)
		log.Printf("║  Spieler aktiv:      %-39d ║", health.ActivePlayers)
	}

	log.Println("╚══════════════════════════════════════════════════════════════╝")
}

// --- HTTP helpers ---

func httpCreateSession(baseURL, sessionID string, maxPlayers int, apiKey string) error {
	body, _ := json.Marshal(map[string]interface{}{
		"session_id":  sessionID,
		"max_players": maxPlayers,
	})
	resp, err := doHTTP("POST", baseURL+"/sessions", body, apiKey)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusConflict {
		return fmt.Errorf("create session: HTTP %d", resp.StatusCode)
	}
	return nil
}

func httpJoinSession(baseURL, sessionID string, playerID uint32, login string, apiKey string) error {
	body, _ := json.Marshal(map[string]interface{}{
		"player_id": playerID,
		"login":     login,
	})
	resp, err := doHTTP("POST", baseURL+"/sessions/"+sessionID+"/join", body, apiKey)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("join session: HTTP %d", resp.StatusCode)
	}
	return nil
}

func doHTTP(method, url string, body []byte, apiKey string) (*http.Response, error) {
	req, err := http.NewRequest(method, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	return http.DefaultClient.Do(req)
}

type healthResponse struct {
	Status         string `json:"status"`
	ActiveSessions int64  `json:"active_sessions"`
	ActivePlayers  int64  `json:"active_players"`
}

func fetchHealth(baseURL string) *healthResponse {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(baseURL + "/health")
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	var result healthResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil
	}
	return &result
}

// --- Formatting helpers ---

func formatNumber(n uint64) string {
	if n >= 1_000_000_000 {
		return fmt.Sprintf("%.2f Mrd", float64(n)/1_000_000_000)
	}
	if n >= 1_000_000 {
		return fmt.Sprintf("%.2f Mio", float64(n)/1_000_000)
	}
	if n >= 1_000 {
		return fmt.Sprintf("%.1f K", float64(n)/1_000)
	}
	return fmt.Sprintf("%d", n)
}

func formatBytes(b uint64) string {
	if b >= 1024*1024*1024 {
		return fmt.Sprintf("%.2f GB", float64(b)/1024/1024/1024)
	}
	if b >= 1024*1024 {
		return fmt.Sprintf("%.2f MB", float64(b)/1024/1024)
	}
	if b >= 1024 {
		return fmt.Sprintf("%.1f KB", float64(b)/1024)
	}
	return fmt.Sprintf("%d B", b)
}
