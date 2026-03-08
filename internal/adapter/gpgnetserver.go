package adapter

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"

	"fafsturmudprelay/internal/protocol"
)

// maxQueuedMessages is the maximum number of messages held in the pre-lobby
// queue.  Prevents unbounded memory growth if the game never reaches Lobby state.
const maxQueuedMessages = 100

// GPGNetServer is a TCP server that the game engine connects to.
// It handles the GPGNet binary protocol for communication with ForgedAlliance.exe.
type GPGNetServer struct {
	listener  net.Listener
	adapter   *Adapter
	logger    *slog.Logger

	connMu    sync.Mutex
	conn      net.Conn
	connEpoch uint64 // incremented on each new connection; guards cleanup
	writer    *protocol.GPGNetWriter
	connected bool

	gameState    string
	lobbyReady   chan struct{}
	lobbyOnce    sync.Once
	messageQueue []queuedMessage

	queueMu sync.Mutex
}

type queuedMessage struct {
	header string
	args   []interface{}
}

// NewGPGNetServer creates a new GPGNet server listening on the specified port.
func NewGPGNetServer(port int, adapter *Adapter, logger *slog.Logger) (*GPGNetServer, error) {
	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return nil, err
	}

	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}

	return &GPGNetServer{
		listener:   listener,
		adapter:    adapter,
		logger:     logger,
		gameState:  protocol.GameStateNone,
		lobbyReady: make(chan struct{}),
	}, nil
}

// Port returns the TCP port the server is listening on.
func (g *GPGNetServer) Port() int {
	addr, ok := g.listener.Addr().(*net.TCPAddr)
	if !ok {
		return 0
	}
	return addr.Port
}

// IsConnected returns whether a game is currently connected.
func (g *GPGNetServer) IsConnected() bool {
	g.connMu.Lock()
	defer g.connMu.Unlock()
	return g.connected
}

// GameState returns the current game state.
func (g *GPGNetServer) GameState() string {
	g.connMu.Lock()
	defer g.connMu.Unlock()
	return g.gameState
}

// Run accepts game connections. Blocks until context is cancelled.
func (g *GPGNetServer) Run(ctx context.Context) {
	go func() {
		<-ctx.Done()
		g.listener.Close()
	}()

	for {
		conn, err := g.listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			g.logger.Error("GPGNet accept error", "error", err)
			continue
		}

		g.handleConnection(ctx, conn)
	}
}

func (g *GPGNetServer) handleConnection(ctx context.Context, conn net.Conn) {
	g.connMu.Lock()
	// Close previous connection if any
	if g.conn != nil {
		g.conn.Close()
	}
	g.conn = conn
	g.writer = protocol.NewGPGNetWriter(conn)
	g.connected = true
	g.gameState = protocol.GameStateNone
	g.connEpoch++
	myEpoch := g.connEpoch
	g.connMu.Unlock()

	// Reset lobby state for the new connection so a reconnecting game
	// goes through the full Idle→Lobby flow again.
	g.queueMu.Lock()
	g.lobbyOnce = sync.Once{} // allow lobbyReady to be closed again
	g.lobbyReady = make(chan struct{})
	g.messageQueue = nil
	g.queueMu.Unlock()

	g.logger.Info("Game connected to GPGNet server", "remote", conn.RemoteAddr())

	// Notify FAF client
	g.adapter.NotifyConnectionStateChanged("Connected")

	// Read loop
	connCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	go func() {
		<-connCtx.Done()
		conn.Close()
	}()

	reader := protocol.NewGPGNetReader(conn)
	for {
		header, chunks, err := reader.ReadMessage()
		if err != nil {
			if errors.Is(err, io.EOF) || connCtx.Err() != nil {
				g.logger.Info("Game disconnected from GPGNet")
			} else {
				g.logger.Error("GPGNet read error", "error", err)
			}
			break
		}

		g.processMessage(header, chunks)
	}

	// Only clean up if WE are still the active connection.
	// If a newer connection has taken over (epoch changed), don't touch state.
	g.connMu.Lock()
	if g.connEpoch == myEpoch {
		g.connected = false
		g.conn = nil
		g.writer = nil
	}
	g.connMu.Unlock()

	if g.connEpoch == myEpoch {
		g.adapter.NotifyConnectionStateChanged("Disconnected")
		g.adapter.signalGameDisconnected()
	}
}

// processMessage handles an incoming GPGNet message from the game.
func (g *GPGNetServer) processMessage(header string, chunks []interface{}) {
	g.logger.Debug("GPGNet message from game", "header", header, "chunks", chunks)

	// Forward to FAF client via RPC notification
	g.adapter.NotifyGpgNetMessage(header, chunks)

	switch header {
	case protocol.CmdGameState:
		if len(chunks) > 0 {
			if state, ok := chunks[0].(string); ok {
				g.onGameState(state)
			}
		}
	}
}

// onGameState handles game state transitions.
func (g *GPGNetServer) onGameState(state string) {
	g.logger.Info("Game state changed", "state", state)
	g.connMu.Lock()
	g.gameState = state
	g.connMu.Unlock()

	switch state {
	case protocol.GameStateIdle:
		// Send CreateLobby to the game
		lobbyPort := g.adapter.LobbyPort()
		g.SendToGame(protocol.CmdCreateLobby, []interface{}{
			int32(g.adapter.LobbyInitMode()),
			int32(lobbyPort),
			g.adapter.Login(),
			int32(g.adapter.PlayerID()),
			int32(1), // natTraversalProvider (always 1)
		})

	case protocol.GameStateLobby:
		// Mark lobby as ready — flush queued messages
		g.lobbyOnce.Do(func() {
			close(g.lobbyReady)
		})
		g.flushQueue()
	}
}

// SendToGame sends a GPGNet message to the connected game.
func (g *GPGNetServer) SendToGame(header string, args []interface{}) error {
	g.connMu.Lock()
	defer g.connMu.Unlock()

	if g.writer == nil {
		return fmt.Errorf("no game connected")
	}

	g.logger.Debug("Sending GPGNet to game", "header", header, "args", args)
	return g.writer.WriteMessage(header, args)
}

// SendToGameQueued queues a message to be sent once the lobby is ready.
func (g *GPGNetServer) SendToGameQueued(header string, args []interface{}) {
	g.queueMu.Lock()
	select {
	case <-g.lobbyReady:
		g.queueMu.Unlock()
		// Lobby already ready, send immediately
		if err := g.SendToGame(header, args); err != nil {
			g.logger.Error("Failed to send queued GPGNet message", "header", header, "error", err)
		}
		return
	default:
	}
	if len(g.messageQueue) >= maxQueuedMessages {
		g.logger.Warn("GPGNet message queue full, dropping message", "header", header)
		g.queueMu.Unlock()
		return
	}
	g.messageQueue = append(g.messageQueue, queuedMessage{header: header, args: args})
	g.queueMu.Unlock()
}

// flushQueue sends all queued messages.
func (g *GPGNetServer) flushQueue() {
	g.queueMu.Lock()
	queue := g.messageQueue
	g.messageQueue = nil
	g.queueMu.Unlock()

	for _, msg := range queue {
		if err := g.SendToGame(msg.header, msg.args); err != nil {
			g.logger.Error("Failed to flush queued message", "header", msg.header, "error", err)
		}
	}
}

// Close closes the GPGNet server.
func (g *GPGNetServer) Close() error {
	g.connMu.Lock()
	if g.conn != nil {
		g.conn.Close()
	}
	g.connMu.Unlock()
	return g.listener.Close()
}
