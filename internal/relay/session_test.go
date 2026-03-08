package relay

import (
	"net/netip"
	"testing"
	"time"
)

func TestSessionAddRemovePlayer(t *testing.T) {
	s := NewSession("test", 12)
	if err := s.AddPlayer(1, "player1"); err != nil {
		t.Fatalf("AddPlayer: %v", err)
	}
	if err := s.AddPlayer(2, "player2"); err != nil {
		t.Fatalf("AddPlayer: %v", err)
	}
	if s.PlayerCount() != 2 {
		t.Errorf("PlayerCount = %d, want 2", s.PlayerCount())
	}

	s.RemovePlayer(1)
	if s.PlayerCount() != 1 {
		t.Errorf("PlayerCount = %d, want 1", s.PlayerCount())
	}

	p := s.GetPlayer(2)
	if p == nil || p.Login != "player2" {
		t.Errorf("GetPlayer(2) = %v, want player2", p)
	}
	if s.GetPlayer(1) != nil {
		t.Error("GetPlayer(1) should be nil after removal")
	}
}

func TestSessionDuplicatePlayer(t *testing.T) {
	s := NewSession("test", 12)
	s.AddPlayer(1, "p1")
	if err := s.AddPlayer(1, "p1again"); err == nil {
		t.Error("expected error for duplicate player")
	}
}

func TestSessionFull(t *testing.T) {
	s := NewSession("test", 2)
	s.AddPlayer(1, "p1")
	s.AddPlayer(2, "p2")
	if err := s.AddPlayer(3, "p3"); err == nil {
		t.Error("expected error for full session")
	}
}

func TestSessionUpdateAddr(t *testing.T) {
	s := NewSession("test", 12)
	s.AddPlayer(1, "p1")

	addr := netip.MustParseAddrPort("192.168.1.1:5000")
	s.UpdatePlayerAddr(1, addr, time.Now())

	p := s.GetPlayer(1)
	if !p.Addr.IsValid() || p.Addr.Port() != 5000 {
		t.Errorf("Addr = %v, want port 5000", p.Addr)
	}
	if p.LastSeen.IsZero() {
		t.Error("LastSeen should be set")
	}
}

func TestSessionGetOtherPlayers(t *testing.T) {
	s := NewSession("test", 12)
	s.AddPlayer(1, "p1")
	s.AddPlayer(2, "p2")
	s.AddPlayer(3, "p3")

	others := s.GetOtherPlayers(2)
	if len(others) != 2 {
		t.Fatalf("GetOtherPlayers = %d, want 2", len(others))
	}
	for _, p := range others {
		if p.PlayerID == 2 {
			t.Error("GetOtherPlayers should exclude self")
		}
	}
}

func TestSessionManagerCreateDelete(t *testing.T) {
	m := &Metrics{}
	sm := NewSessionManager(m)

	s, err := sm.CreateSession("game1", 12)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if s.ID != "game1" {
		t.Errorf("ID = %q, want %q", s.ID, "game1")
	}
	if m.ActiveSessions.Load() != 1 {
		t.Errorf("ActiveSessions = %d, want 1", m.ActiveSessions.Load())
	}

	// Duplicate
	_, err = sm.CreateSession("game1", 12)
	if err == nil {
		t.Error("expected error for duplicate session")
	}

	if err := sm.DeleteSession("game1"); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	if m.ActiveSessions.Load() != 0 {
		t.Errorf("ActiveSessions = %d, want 0", m.ActiveSessions.Load())
	}
}

func TestSessionManagerPlayerIndex(t *testing.T) {
	m := &Metrics{}
	sm := NewSessionManager(m)
	sm.CreateSession("game1", 12)

	if err := sm.AddPlayerToSession("game1", 100, "player100"); err != nil {
		t.Fatalf("AddPlayer: %v", err)
	}
	if m.ActivePlayers.Load() != 1 {
		t.Errorf("ActivePlayers = %d, want 1", m.ActivePlayers.Load())
	}

	// O(1) lookup
	s := sm.FindSessionByPlayer(100)
	if s == nil || s.ID != "game1" {
		t.Errorf("FindSessionByPlayer(100) = %v, want game1", s)
	}

	// Unknown player
	if sm.FindSessionByPlayer(999) != nil {
		t.Error("FindSessionByPlayer(999) should be nil")
	}

	// Remove player
	sm.RemovePlayerFromSession("game1", 100)
	if sm.FindSessionByPlayer(100) != nil {
		t.Error("FindSessionByPlayer(100) should be nil after removal")
	}
	if m.ActivePlayers.Load() != 0 {
		t.Errorf("ActivePlayers = %d, want 0", m.ActivePlayers.Load())
	}
}

func TestSessionManagerDeleteCleansIndex(t *testing.T) {
	m := &Metrics{}
	sm := NewSessionManager(m)
	sm.CreateSession("game1", 12)
	sm.AddPlayerToSession("game1", 1, "p1")
	sm.AddPlayerToSession("game1", 2, "p2")

	sm.DeleteSession("game1")

	if sm.FindSessionByPlayer(1) != nil {
		t.Error("player 1 should be gone after session delete")
	}
	if sm.FindSessionByPlayer(2) != nil {
		t.Error("player 2 should be gone after session delete")
	}
	if m.ActivePlayers.Load() != 0 {
		t.Errorf("ActivePlayers = %d, want 0", m.ActivePlayers.Load())
	}
}

func TestSessionManagerPlayerMigration(t *testing.T) {
	m := &Metrics{}
	sm := NewSessionManager(m)
	sm.CreateSession("game1", 12)
	sm.CreateSession("game2", 12)

	sm.AddPlayerToSession("game1", 1, "p1")

	// Player migrates to game2 (e.g., adapter restarted with new game-id)
	if err := sm.AddPlayerToSession("game2", 1, "p1"); err != nil {
		t.Fatalf("migration should succeed: %v", err)
	}

	// Player should now be in game2, not game1
	s := sm.FindSessionByPlayer(1)
	if s == nil || s.ID != "game2" {
		t.Errorf("player should be in game2, got %v", s)
	}

	// game1 should have 0 players
	g1 := sm.GetSession("game1")
	if g1.PlayerCount() != 0 {
		t.Errorf("game1 should be empty, has %d players", g1.PlayerCount())
	}

	if m.ActivePlayers.Load() != 1 {
		t.Errorf("ActivePlayers = %d, want 1", m.ActivePlayers.Load())
	}
}

func TestSessionManagerRemoveMissingPlayer(t *testing.T) {
	m := &Metrics{}
	sm := NewSessionManager(m)
	sm.CreateSession("game1", 12)
	sm.AddPlayerToSession("game1", 1, "p1")

	// Remove an existing player — should succeed
	if err := sm.RemovePlayerFromSession("game1", 1); err != nil {
		t.Fatalf("RemovePlayerFromSession: %v", err)
	}
	if m.ActivePlayers.Load() != 0 {
		t.Errorf("ActivePlayers = %d, want 0", m.ActivePlayers.Load())
	}

	// Remove same player again — should return error, metrics unchanged
	if err := sm.RemovePlayerFromSession("game1", 1); err == nil {
		t.Error("expected error when removing non-existing player")
	}
	if m.ActivePlayers.Load() != 0 {
		t.Errorf("ActivePlayers = %d after double remove, want 0", m.ActivePlayers.Load())
	}

	// Remove player that was never added — should return error, metrics unchanged
	if err := sm.RemovePlayerFromSession("game1", 999); err == nil {
		t.Error("expected error when removing player never added")
	}
	if m.ActivePlayers.Load() != 0 {
		t.Errorf("ActivePlayers = %d after removing unknown player, want 0", m.ActivePlayers.Load())
	}
}

func TestSessionRemovePlayerReturnValue(t *testing.T) {
	s := NewSession("test", 12)
	s.AddPlayer(1, "p1")

	if !s.RemovePlayer(1) {
		t.Error("RemovePlayer should return true for existing player")
	}
	if s.RemovePlayer(1) {
		t.Error("RemovePlayer should return false for non-existing player")
	}
}

func TestSessionManagerIdempotentJoin(t *testing.T) {
	m := &Metrics{}
	sm := NewSessionManager(m)
	sm.CreateSession("game1", 12)

	sm.AddPlayerToSession("game1", 1, "p1")

	// Joining same session again should succeed (idempotent)
	if err := sm.AddPlayerToSession("game1", 1, "p1"); err != nil {
		t.Fatalf("idempotent join should succeed: %v", err)
	}

	if m.ActivePlayers.Load() != 1 {
		t.Errorf("ActivePlayers = %d, want 1", m.ActivePlayers.Load())
	}
}
