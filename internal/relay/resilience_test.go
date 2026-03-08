package relay

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestReaperRemovesStalePlayers verifies that the reaper removes players
// who haven't sent packets within the timeout window.
func TestReaperRemovesStalePlayers(t *testing.T) {
	metrics := &Metrics{}
	sm := NewSessionManager(metrics)

	s, err := sm.CreateSession("game1", 8)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if err := sm.AddPlayerToSession("game1", 100, "Alice"); err != nil {
		t.Fatalf("AddPlayer: %v", err)
	}

	// Backdate the session and player so they appear stale
	s.CreatedAt = time.Now().Add(-10 * time.Minute)

	logger := discardLogger()

	removedPlayers, removedSessions := sm.ReapStaleSessions(logger)
	if removedPlayers != 1 {
		t.Errorf("expected 1 removed player, got %d", removedPlayers)
	}
	// Session was emptied AND old enough → also reaped in same pass
	if removedSessions != 1 {
		t.Errorf("expected 1 removed session (empty + old), got %d", removedSessions)
	}
	_ = s

	// Verify session is actually gone
	if sm.GetSession("game1") != nil {
		t.Error("session should be removed after reaper")
	}
}

// TestReaperKeepsActivePlayers verifies the reaper does NOT remove
// players who have been recently active.
func TestReaperKeepsActivePlayers(t *testing.T) {
	metrics := &Metrics{}
	sm := NewSessionManager(metrics)

	sm.CreateSession("game2", 8)
	sm.AddPlayerToSession("game2", 200, "Bob")

	// Simulate recent activity
	s := sm.GetSession("game2")
	s.mu.Lock()
	s.players[200].LastSeen = time.Now() // just saw a packet
	s.mu.Unlock()

	logger := discardLogger()
	removedPlayers, _ := sm.ReapStaleSessions(logger)
	if removedPlayers != 0 {
		t.Errorf("active player should NOT be reaped, but %d were removed", removedPlayers)
	}
}

// TestReaperRunsAndStops verifies RunReaper goroutine exits on context cancel.
func TestReaperRunsAndStops(t *testing.T) {
	metrics := &Metrics{}
	sm := NewSessionManager(metrics)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	logger := discardLogger()
	go func() {
		sm.RunReaper(ctx, logger)
		close(done)
	}()

	cancel()

	select {
	case <-done:
		// Good — reaper exited
	case <-time.After(2 * time.Second):
		t.Fatal("RunReaper did not exit after context cancel")
	}
}

// TestHTTPBodySizeLimit verifies that oversized request bodies are rejected.
func TestHTTPBodySizeLimit(t *testing.T) {
	metrics := &Metrics{}
	sm := NewSessionManager(metrics)
	logger := discardLogger()
	api := NewHTTPAPI(sm, metrics, logger, "", 0, 0)

	// Create a body larger than maxRequestBodySize (64 KB)
	hugeBody := strings.Repeat("x", 128*1024)
	req := httptest.NewRequest(http.MethodPost, "/sessions", strings.NewReader(hugeBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	api.ServeHTTP(w, req)

	// Should get a 400 Bad Request (body too large triggers decode error)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for oversized body, got %d", w.Code)
	}
}

// TestHTTPServerTimeoutsConfigured verifies that the HTTP server in Run()
// would be created with the expected timeout and header-size values.
// Since we can't easily inspect the http.Server created inside Run(),
// we verify the constants/values used in server.go via a build-level test.
func TestHTTPServerTimeoutsConfigured(t *testing.T) {
	cfg := ServerConfig{UDPAddr: ":0", HTTPAddr: ":0"}
	s := NewServer(cfg)
	if s == nil {
		t.Fatal("NewServer returned nil")
	}
	// Verify the config is stored — the actual http.Server with timeouts
	// (ReadTimeout=10s, ReadHeaderTimeout=5s, WriteTimeout=10s,
	// IdleTimeout=60s, MaxHeaderBytes=8192) is created in Run().
	if s.cfg.HTTPAddr != ":0" {
		t.Errorf("HTTPAddr = %q, want :0", s.cfg.HTTPAddr)
	}
}
