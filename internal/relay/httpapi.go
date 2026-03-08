package relay

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

// maxRequestBodySize limits HTTP request bodies to 64 KB to prevent OOM.
const maxRequestBodySize = 64 * 1024

// maxSessionPlayers caps max_players per session to prevent resource abuse.
// 32 = 16 aktive Spieler + Reserve für Observer und gekickte Spieler,
// deren FA.exe noch nicht geschlossen wurde.
const maxSessionPlayers = 32

// sessionIDPattern validates session IDs: alphanumeric, hyphens, underscores, max 64 chars.
var sessionIDPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

// httpRateLimit is a simple global rate limiter for the HTTP API.
// Uses a mutex to avoid TOCTOU races between second-boundary resets and counting.
type httpRateLimit struct {
	mu      sync.Mutex
	count   uint64
	second  int64
	maxRate uint64 // max requests per second
}

// Allow checks if a request is within the rate limit.
func (rl *httpRateLimit) Allow() bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	now := time.Now().Unix()
	if rl.second != now {
		rl.second = now
		rl.count = 0
	}
	rl.count++
	return rl.count <= rl.maxRate
}

// perIPLimit is a per-source-IP rate limiter for the HTTP API.
// It prevents a single IP from monopolising the global rate budget or
// flooding the session/player tables.
type perIPLimit struct {
	mu      sync.Mutex
	buckets map[string]*ipBucket
}

type ipBucket struct {
	count  uint64
	second int64
}

func newPerIPLimit() *perIPLimit {
	return &perIPLimit{buckets: make(map[string]*ipBucket)}
}

// Allow checks whether the given IP is within the per-IP rate limit.
// When the map exceeds 10 000 entries, stale buckets (older than 1 s) are
// removed selectively rather than wiping the entire map — this avoids
// simultaneously resetting all rate limits on a hot server.
func (l *perIPLimit) Allow(ip string, maxRate uint64) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now().Unix()
	if len(l.buckets) > 10000 {
		for k, b := range l.buckets {
			if b.second < now-1 {
				delete(l.buckets, k)
			}
		}
	}
	b, ok := l.buckets[ip]
	if !ok {
		b = &ipBucket{}
		l.buckets[ip] = b
	}
	if b.second != now {
		b.second = now
		b.count = 0
	}
	b.count++
	return b.count <= maxRate
}

// extractClientIP returns only the host part of r.RemoteAddr.
func extractClientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// HTTPAPI provides REST endpoints for session management.
type HTTPAPI struct {
	sm            *SessionManager
	metrics       *Metrics
	logger        *slog.Logger
	mux           *http.ServeMux
	rateLimit     *httpRateLimit // mutating endpoints (POST/DELETE)
	readRateLimit *httpRateLimit // read-only endpoints (GET) — generous limit to prevent flooding
	perIPLimit    *perIPLimit
	perIPRate     uint64 // max mutating requests per second per IP
	apiKey        string // if non-empty, mutating endpoints require Authorization: Bearer <key>
}

// NewHTTPAPI creates a new HTTP API handler.
// globalRate and perIPRate override the default rate limits when non-zero.
func NewHTTPAPI(sm *SessionManager, metrics *Metrics, logger *slog.Logger, apiKey string, globalRate, perIPRate uint64) *HTTPAPI {
	if globalRate == 0 {
		globalRate = 200
	}
	if perIPRate == 0 {
		perIPRate = 20
	}
	api := &HTTPAPI{
		sm:            sm,
		metrics:       metrics,
		logger:        logger,
		mux:           http.NewServeMux(),
		rateLimit:     &httpRateLimit{maxRate: globalRate},
		readRateLimit: &httpRateLimit{maxRate: 2000}, // 10× more generous than mutating
		perIPLimit:    newPerIPLimit(),
		perIPRate:     perIPRate,
		apiKey:        apiKey,
	}
	api.mux.HandleFunc("/health", api.handleHealth)
	api.mux.HandleFunc("/sessions", api.handleSessions)
	api.mux.HandleFunc("/sessions/", api.handleSessionByID)
	return api
}

// ServeHTTP implements http.Handler.
func (a *HTTPAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodySize)
	// Mutating endpoints (POST/DELETE) require API key and are rate-limited.
	// GET endpoints use a separate, more generous rate limit (no API key).
	if r.Method != http.MethodGet {
		if !a.checkAPIKey(r) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if !a.rateLimit.Allow() {
			http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
			return
		}
		// Per-IP rate limit: prevents a single client from monopolising the
		// global budget or flooding session/player tables.
		if !a.perIPLimit.Allow(extractClientIP(r), a.perIPRate) {
			http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
			return
		}
	} else {
		if !a.readRateLimit.Allow() {
			http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
			return
		}
	}
	a.mux.ServeHTTP(w, r)
}

// checkAPIKey validates the Authorization header if an API key is configured.
// Returns true if no API key is configured or if the key matches.
//
// Security: uses crypto/subtle.ConstantTimeCompare to prevent timing side-channel
// attacks where an attacker measures response latency to guess the key one byte
// at a time.
func (a *HTTPAPI) checkAPIKey(r *http.Request) bool {
	if a.apiKey == "" {
		return true // no auth configured
	}
	auth := r.Header.Get("Authorization")
	expected := "Bearer " + a.apiKey
	return subtle.ConstantTimeCompare([]byte(auth), []byte(expected)) == 1
}

// decodeJSONBody decodes a JSON request body with strict validation.
// Rejects unknown fields and trailing data after the first JSON object.
func decodeJSONBody(body io.Reader, v interface{}) error {
	dec := json.NewDecoder(body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("unexpected trailing JSON data")
	}
	return nil
}

func (a *HTTPAPI) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	snap := a.metrics.Snapshot()
	// Only expose minimal info publicly — no internal counters that help attackers.
	resp := struct {
		Status         string `json:"status"`
		ActiveSessions int64  `json:"active_sessions"`
		ActivePlayers  int64  `json:"active_players"`
	}{
		Status:         "ok",
		ActiveSessions: snap.ActiveSessions,
		ActivePlayers:  snap.ActivePlayers,
	}
	writeJSON(w, http.StatusOK, resp)
}

func (a *HTTPAPI) handleSessions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		SessionID  string `json:"session_id"`
		MaxPlayers int    `json:"max_players"`
	}
	if err := decodeJSONBody(r.Body, &req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.SessionID == "" {
		http.Error(w, "session_id required", http.StatusBadRequest)
		return
	}
	if !sessionIDPattern.MatchString(req.SessionID) {
		http.Error(w, "session_id must be alphanumeric/hyphens/underscores, max 64 chars", http.StatusBadRequest)
		return
	}
	if req.MaxPlayers <= 0 {
		req.MaxPlayers = 12
	}
	if req.MaxPlayers > maxSessionPlayers {
		req.MaxPlayers = maxSessionPlayers
	}

	session, err := a.sm.CreateSession(req.SessionID, req.MaxPlayers)
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}

	a.logger.Info("Session created", "session_id", session.ID, "max_players", session.MaxPlayers)
	writeJSON(w, http.StatusCreated, map[string]interface{}{
		"session_id":  session.ID,
		"max_players": session.MaxPlayers,
	})
}

// handleSessionByID routes /sessions/{id}, /sessions/{id}/join, /sessions/{id}/leave
func (a *HTTPAPI) handleSessionByID(w http.ResponseWriter, r *http.Request) {
	// Parse path: /sessions/{id}[/action]
	path := strings.TrimPrefix(r.URL.Path, "/sessions/")
	parts := strings.SplitN(path, "/", 2)
	sessionID := parts[0]
	action := ""
	if len(parts) > 1 {
		action = parts[1]
	}

	if sessionID == "" {
		http.Error(w, "session_id required in path", http.StatusBadRequest)
		return
	}
	if !sessionIDPattern.MatchString(sessionID) {
		http.Error(w, "invalid session_id", http.StatusBadRequest)
		return
	}

	switch action {
	case "":
		a.handleSessionRoot(w, r, sessionID)
	case "join":
		a.handleSessionJoin(w, r, sessionID)
	case "leave":
		a.handleSessionLeave(w, r, sessionID)
	default:
		http.Error(w, "unknown action", http.StatusNotFound)
	}
}

func (a *HTTPAPI) handleSessionRoot(w http.ResponseWriter, r *http.Request, sessionID string) {
	switch r.Method {
	case http.MethodGet:
		session := a.sm.GetSession(sessionID)
		if session == nil {
			http.Error(w, "session not found", http.StatusNotFound)
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"session_id":   session.ID,
			"max_players":  session.MaxPlayers,
			"player_count": session.PlayerCount(),
			"player_ids":   session.PlayerIDs(),
		})
	case http.MethodDelete:
		if err := a.sm.DeleteSession(sessionID); err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		a.logger.Info("Session deleted", "session_id", sessionID)
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (a *HTTPAPI) handleSessionJoin(w http.ResponseWriter, r *http.Request, sessionID string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		PlayerID uint32 `json:"player_id"`
		Login    string `json:"login"`
	}
	if err := decodeJSONBody(r.Body, &req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.PlayerID == 0 {
		http.Error(w, "player_id required", http.StatusBadRequest)
		return
	}

	// Check if player is migrating from another session (for logging)
	oldSession := a.sm.FindSessionByPlayer(req.PlayerID)
	var oldSessionID string
	if oldSession != nil && oldSession.ID != sessionID {
		oldSessionID = oldSession.ID
	}

	if err := a.sm.AddPlayerToSession(sessionID, req.PlayerID, req.Login); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}

	if oldSessionID != "" {
		a.logger.Info("Player migrated to new session",
			"session_id", sessionID, "player_id", req.PlayerID,
			"login", req.Login, "old_session", oldSessionID)
	} else {
		a.logger.Info("Player joined session", "session_id", sessionID, "player_id", req.PlayerID, "login", req.Login)
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"session_id": sessionID,
		"player_id":  req.PlayerID,
	})
}

func (a *HTTPAPI) handleSessionLeave(w http.ResponseWriter, r *http.Request, sessionID string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		PlayerID uint32 `json:"player_id"`
	}
	if err := decodeJSONBody(r.Body, &req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if req.PlayerID == 0 {
		http.Error(w, "player_id required", http.StatusBadRequest)
		return
	}

	if err := a.sm.RemovePlayerFromSession(sessionID, req.PlayerID); err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	a.logger.Info("Player left session", "session_id", sessionID, "player_id", req.PlayerID)
	w.WriteHeader(http.StatusNoContent)
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
