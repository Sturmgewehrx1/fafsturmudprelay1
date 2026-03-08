package adapter

import (
	_ "embed"
	"encoding/json"
	"net"
	"net/http"
	"os/exec"
	"strconv"
)

//go:embed debug_ui.html
var debugUIHTML []byte

// DebugServer serves a browser-based debug status page on a random loopback port.
type DebugServer struct {
	adapter *Adapter
	port    int
	server  *http.Server
}

// NewDebugServer creates and starts a debug HTTP server on a random 127.0.0.1 port.
func NewDebugServer(a *Adapter) (*DebugServer, error) {
	mux := http.NewServeMux()

	mux.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
		status := a.BuildStatus()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(status) //nolint:errcheck
	})

	mux.HandleFunc("/api/reconnect", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		pidStr := r.URL.Query().Get("player_id")
		pid, err := strconv.Atoi(pidStr)
		if err != nil || pid <= 0 {
			http.Error(w, "invalid player_id", http.StatusBadRequest)
			return
		}
		if err := a.ForceICEReconnect(pid); err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(debugUIHTML) //nolint:errcheck
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}

	srv := &http.Server{Handler: mux}
	d := &DebugServer{
		adapter: a,
		port:    ln.Addr().(*net.TCPAddr).Port,
		server:  srv,
	}
	go srv.Serve(ln) //nolint:errcheck
	return d, nil
}

// Port returns the TCP port the debug server is listening on.
func (d *DebugServer) Port() int { return d.port }

// Close shuts down the debug HTTP server.
func (d *DebugServer) Close() { d.server.Close() }

// OpenBrowser opens the default Windows browser at the given URL.
// Runs asynchronously; failure is silently ignored (Windows-only target).
func OpenBrowser(url string) {
	exec.Command("cmd", "/c", "start", url).Start() //nolint:errcheck
}
