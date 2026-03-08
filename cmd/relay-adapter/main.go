package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"fafsturmudprelay/internal/adapter"
)

// Build-time defaults — override with:
//
//	go build -ldflags "-X main.defaultRelayServer=myserver:10000 -X main.defaultRelayHTTP=http://myserver:8080"
var (
	defaultRelayServer    = "91.134.10.126:10000"       // UDP relay address
	defaultRelayTCPServer = "91.134.10.126:443"          // TCP fallback address (port 443 passes most firewalls)
	defaultRelayHTTP      = "http://91.134.10.126:8080" // HTTP API address

	// defaultForceTCP: true  = always use TCP (skip UDP probe)
	//                  false = try UDP first, fall back to TCP on timeout (5 s)
	defaultForceTCP = false
)

func main() {
	// Required flags
	playerID := flag.Int("id", 0, "Player ID (required)")
	gameID := flag.Int("game-id", 0, "Game ID (required)")
	login := flag.String("login", "", "Player login (required)")

	// Optional flags
	rpcPort := flag.Int("rpc-port", 7236, "JSON-RPC listen port")
	gpgnetPort := flag.Int("gpgnet-port", 0, "GPGNet listen port (0 = auto)")
	lobbyPort := flag.Int("lobby-port", 0, "Lobby UDP port (0 = auto)")
	relayServer := flag.String("relay-server", defaultRelayServer, "Relay server UDP address (host:port)")
	relayTCPServer := flag.String("relay-tcp", defaultRelayTCPServer, "Relay server TCP fallback address (host:port)")
	relayHTTP := flag.String("relay-http", defaultRelayHTTP, "Relay server HTTP address (http://host:port)")
	apiKey := flag.String("api-key", "mussfuerproduktiongeandertwerdenxyz45k8", "API key for relay server HTTP endpoints")
	tcpFallback := flag.Bool("tcp-fallback", true, "Fall back to TCP when UDP is unreachable")
	forceTCP := flag.Bool("force-tcp", defaultForceTCP, "Always use TCP (skip UDP probe; overrides --tcp-fallback)")

	debugWindow := flag.Bool("debug-window", false, "Open browser debug window")
	infoWindow := flag.Bool("info-window", false, "Open browser debug window (alias for --debug-window)")

	// Accepted and ignored flags (java-ice-adapter compatibility)
	flag.Bool("force-relay", false, "(ignored)")
	flag.Int("delay-ui", 0, "(ignored)")
	flag.Int("ping-count", 1, "(ignored)")
	flag.Float64("acceptable-latency", 250.0, "(ignored)")
	flag.String("telemetry-server", "", "(ignored)")
	flag.String("access-token", "", "(ignored)")
	flag.String("icebreaker-base-url", "", "(ignored)")

	// Parse flags — unknown flags are silently ignored (matching picocli behavior)
	flag.CommandLine.SetOutput(os.Stderr)
	flag.Parse()

	if *playerID == 0 || *login == "" {
		fmt.Fprintln(os.Stderr, "Required: --id, --login")
		flag.Usage()
		os.Exit(1)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		cancel()
	}()

	cfg := adapter.Config{
		PlayerID:       *playerID,
		GameID:         *gameID,
		Login:          *login,
		RPCPort:        *rpcPort,
		GPGNetPort:     *gpgnetPort,
		LobbyPort:      *lobbyPort,
		RelayServer:    *relayServer,
		RelayTCPServer: *relayTCPServer,
		RelayHTTP:      *relayHTTP,
		APIKey:         *apiKey,
		TCPFallback:    *tcpFallback,
		ForceTCP:       *forceTCP,
		DebugWindow:    *debugWindow || *infoWindow,
	}

	// Load optional config file (faf-sturm-relay.conf) from the same
	// directory as the executable. Missing file is silently ignored.
	slogLogger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	adapter.LoadConfigFile(&cfg, slogLogger)

	a := adapter.NewAdapter(cfg)
	if err := a.Run(ctx); err != nil {
		log.Fatalf("adapter: %v", err)
	}
}
