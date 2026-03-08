package relay

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

func setupAPI(t *testing.T) *HTTPAPI {
	t.Helper()
	metrics := &Metrics{}
	sm := NewSessionManager(metrics)
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	return NewHTTPAPI(sm, metrics, logger, "", 0, 0)
}

func TestAPIHealthEndpoint(t *testing.T) {
	api := setupAPI(t)

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()
	api.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var resp struct {
		Status string `json:"status"`
	}
	json.NewDecoder(w.Body).Decode(&resp)
	if resp.Status != "ok" {
		t.Errorf("status = %q, want %q", resp.Status, "ok")
	}
}

func TestAPICreateSession(t *testing.T) {
	api := setupAPI(t)

	body := bytes.NewBufferString(`{"session_id":"game1","max_players":8}`)
	req := httptest.NewRequest(http.MethodPost, "/sessions", body)
	w := httptest.NewRecorder()
	api.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d. body: %s", w.Code, http.StatusCreated, w.Body.String())
	}

	var resp map[string]interface{}
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["session_id"] != "game1" {
		t.Errorf("session_id = %v, want game1", resp["session_id"])
	}
}

func TestAPICreateSessionDuplicate(t *testing.T) {
	api := setupAPI(t)

	body := `{"session_id":"game1","max_players":8}`
	req := httptest.NewRequest(http.MethodPost, "/sessions", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	api.ServeHTTP(w, req)

	// Create again
	req2 := httptest.NewRequest(http.MethodPost, "/sessions", bytes.NewBufferString(body))
	w2 := httptest.NewRecorder()
	api.ServeHTTP(w2, req2)

	if w2.Code != http.StatusConflict {
		t.Fatalf("duplicate status = %d, want %d", w2.Code, http.StatusConflict)
	}
}

func TestAPIJoinSession(t *testing.T) {
	api := setupAPI(t)

	// Create session
	createBody := `{"session_id":"game1","max_players":12}`
	api.ServeHTTP(httptest.NewRecorder(),
		httptest.NewRequest(http.MethodPost, "/sessions", bytes.NewBufferString(createBody)))

	// Join
	joinBody := `{"player_id":42,"login":"TestPlayer"}`
	req := httptest.NewRequest(http.MethodPost, "/sessions/game1/join", bytes.NewBufferString(joinBody))
	w := httptest.NewRecorder()
	api.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d. body: %s", w.Code, http.StatusOK, w.Body.String())
	}
}

func TestAPILeaveSession(t *testing.T) {
	api := setupAPI(t)

	// Create + join
	api.ServeHTTP(httptest.NewRecorder(),
		httptest.NewRequest(http.MethodPost, "/sessions",
			bytes.NewBufferString(`{"session_id":"game1","max_players":12}`)))
	api.ServeHTTP(httptest.NewRecorder(),
		httptest.NewRequest(http.MethodPost, "/sessions/game1/join",
			bytes.NewBufferString(`{"player_id":42,"login":"p"}`)))

	// Leave
	leaveBody := `{"player_id":42}`
	req := httptest.NewRequest(http.MethodPost, "/sessions/game1/leave", bytes.NewBufferString(leaveBody))
	w := httptest.NewRecorder()
	api.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusNoContent)
	}
}

func TestAPIGetSession(t *testing.T) {
	api := setupAPI(t)

	// Create + add players
	api.ServeHTTP(httptest.NewRecorder(),
		httptest.NewRequest(http.MethodPost, "/sessions",
			bytes.NewBufferString(`{"session_id":"game1","max_players":12}`)))
	api.ServeHTTP(httptest.NewRecorder(),
		httptest.NewRequest(http.MethodPost, "/sessions/game1/join",
			bytes.NewBufferString(`{"player_id":1,"login":"p1"}`)))
	api.ServeHTTP(httptest.NewRecorder(),
		httptest.NewRequest(http.MethodPost, "/sessions/game1/join",
			bytes.NewBufferString(`{"player_id":2,"login":"p2"}`)))

	req := httptest.NewRequest(http.MethodGet, "/sessions/game1", nil)
	w := httptest.NewRecorder()
	api.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var resp map[string]interface{}
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["player_count"].(float64) != 2 {
		t.Errorf("player_count = %v, want 2", resp["player_count"])
	}
}

func TestAPIRejectsUnknownJSONFields(t *testing.T) {
	api := setupAPI(t)

	// Unknown field "hacker_field" should be rejected
	body := bytes.NewBufferString(`{"session_id":"game1","max_players":8,"hacker_field":"evil"}`)
	req := httptest.NewRequest(http.MethodPost, "/sessions", body)
	w := httptest.NewRecorder()
	api.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for unknown JSON field, got %d", w.Code)
	}
}

func TestAPIRejectsTrailingJSONData(t *testing.T) {
	api := setupAPI(t)

	// Two JSON objects concatenated — only the first would be parsed without trailing check
	body := bytes.NewBufferString(`{"session_id":"game1","max_players":8}{"session_id":"game2","max_players":8}`)
	req := httptest.NewRequest(http.MethodPost, "/sessions", body)
	w := httptest.NewRecorder()
	api.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for trailing JSON data, got %d", w.Code)
	}
}

func TestAPILeaveRejectsZeroPlayerID(t *testing.T) {
	api := setupAPI(t)

	// Create + join
	api.ServeHTTP(httptest.NewRecorder(),
		httptest.NewRequest(http.MethodPost, "/sessions",
			bytes.NewBufferString(`{"session_id":"game1","max_players":12}`)))
	api.ServeHTTP(httptest.NewRecorder(),
		httptest.NewRequest(http.MethodPost, "/sessions/game1/join",
			bytes.NewBufferString(`{"player_id":42,"login":"p"}`)))

	// Leave with player_id 0 should be rejected
	leaveBody := `{"player_id":0}`
	req := httptest.NewRequest(http.MethodPost, "/sessions/game1/leave", bytes.NewBufferString(leaveBody))
	w := httptest.NewRecorder()
	api.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for player_id 0, got %d", w.Code)
	}
}

func TestAPIDeleteSession(t *testing.T) {
	api := setupAPI(t)

	api.ServeHTTP(httptest.NewRecorder(),
		httptest.NewRequest(http.MethodPost, "/sessions",
			bytes.NewBufferString(`{"session_id":"game1","max_players":12}`)))

	req := httptest.NewRequest(http.MethodDelete, "/sessions/game1", nil)
	w := httptest.NewRecorder()
	api.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusNoContent)
	}

	// Verify it's gone
	req2 := httptest.NewRequest(http.MethodGet, "/sessions/game1", nil)
	w2 := httptest.NewRecorder()
	api.ServeHTTP(w2, req2)
	if w2.Code != http.StatusNotFound {
		t.Fatalf("deleted session status = %d, want %d", w2.Code, http.StatusNotFound)
	}
}
