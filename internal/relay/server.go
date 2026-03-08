package relay

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"
)

// ServerConfig holds relay server configuration.
type ServerConfig struct {
	UDPAddr  string // e.g. ":10000"
	TCPAddr  string // e.g. ":10000" — TCP fallback listener (empty = disabled)
	HTTPAddr string // e.g. ":8080"
	APIKey   string // if non-empty, mutating HTTP endpoints require this key
	TLSCert  string // path to TLS certificate PEM file (enables HTTPS when set)
	TLSKey   string // path to TLS private key PEM file (required when TLSCert is set)

	// Rate limits for the HTTP API (0 = use defaults: 100 global, 10 per-IP).
	RateLimitGlobal uint64
	RateLimitPerIP  uint64
}

// Server is the relay server orchestrator.
type Server struct {
	cfg     ServerConfig
	metrics *Metrics
	sm      *SessionManager
	logger  *slog.Logger
	logFile *os.File // non-nil when a log file was successfully created
}

// NewServer creates a new relay server.
func NewServer(cfg ServerConfig) *Server {
	var logWriter io.Writer = os.Stderr
	var logFile *os.File

	// Always create a new log file with timestamp in the name
	logName := fmt.Sprintf("relay%s.log", time.Now().Format("02_01_2006_15_04"))
	f, err := os.Create(logName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "WARNING: cannot create log file %s: %v (logging to stderr only)\n", logName, err)
	} else {
		logFile = f
		logWriter = io.MultiWriter(os.Stderr, f)
		fmt.Fprintf(os.Stderr, "Logging to %s\n", logName)
	}

	logger := slog.New(slog.NewTextHandler(logWriter, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
	metrics := &Metrics{}
	return &Server{
		cfg:     cfg,
		metrics: metrics,
		sm:      NewSessionManager(metrics),
		logger:  logger,
		logFile: logFile,
	}
}

// Run starts the relay server. Blocks until context is cancelled.
func (s *Server) Run(ctx context.Context) error {
	// Close the log file when the server stops so the OS file handle is not leaked
	// across process lifetime (matters when the binary is kept warm between games).
	if s.logFile != nil {
		defer s.logFile.Close()
	}

	// Start UDP listener
	udpAddr, err := net.ResolveUDPAddr("udp", s.cfg.UDPAddr)
	if err != nil {
		return fmt.Errorf("resolving UDP address: %w", err)
	}
	udpConn, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		return fmt.Errorf("listening on UDP: %w", err)
	}
	defer udpConn.Close()

	s.logger.Info("UDP relay listening", "addr", udpConn.LocalAddr().String())

	// Start HTTP API
	api := NewHTTPAPI(s.sm, s.metrics, s.logger, s.cfg.APIKey, s.cfg.RateLimitGlobal, s.cfg.RateLimitPerIP)
	httpServer := &http.Server{
		Addr:              s.cfg.HTTPAddr,
		Handler:           api,
		ReadTimeout:       10 * time.Second,
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    8192,
	}

	// Run HTTP server in background.
	// TLS is used when TLSCert is set; this encrypts the API key in transit.
	httpErrCh := make(chan error, 1)
	go func() {
		s.logger.Info("HTTP API listening", "addr", s.cfg.HTTPAddr, "tls", s.cfg.TLSCert != "")
		var serveErr error
		if s.cfg.TLSCert != "" {
			serveErr = httpServer.ListenAndServeTLS(s.cfg.TLSCert, s.cfg.TLSKey)
		} else {
			serveErr = httpServer.ListenAndServe()
		}
		if serveErr != nil && serveErr != http.ErrServerClosed {
			httpErrCh <- serveErr
		}
		close(httpErrCh)
	}()

	// Create the shared router (used by both UDP and TCP paths).
	router := NewRouter(udpConn, s.sm, s.metrics, s.logger)

	// Start TCP fallback listener (optional).
	// The TCPHandler reuses the shared router so that routing logic and metrics
	// are consistent regardless of whether the peer is on UDP or TCP.
	if s.cfg.TCPAddr != "" {
		tcpListener, err := net.Listen("tcp", s.cfg.TCPAddr)
		if err != nil {
			return fmt.Errorf("listening on TCP: %w", err)
		}
		defer tcpListener.Close()
		s.logger.Info("TCP fallback listening", "addr", tcpListener.Addr().String())
		tcpHandler := NewTCPHandler(tcpListener, router, s.sm, s.metrics, s.logger)
		go tcpHandler.Run(ctx)
	}

	// Run UDP router in background
	routerDone := make(chan struct{})
	go func() {
		router.Run(ctx)
		close(routerDone)
	}()

	// Run session/player reaper in background
	go s.sm.RunReaper(ctx, s.logger)

	// Wait for shutdown
	select {
	case <-ctx.Done():
		s.logger.Info("Shutting down relay server")
	case err := <-httpErrCh:
		return fmt.Errorf("HTTP server error: %w", err)
	}

	// Graceful HTTP shutdown: give in-flight requests 5 s to finish before
	// hard-closing.  Shutdown() (unlike Close()) waits for active handlers.
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		s.logger.Warn("HTTP server shutdown error", "error", err)
	}

	udpConn.Close()
	<-routerDone

	s.logger.Info("Relay server stopped", "metrics", s.metrics.Snapshot())
	return nil
}
