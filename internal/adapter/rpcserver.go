package adapter

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"

	"fafsturmudprelay/internal/protocol"
)

// maxRPCClients is the maximum number of concurrent FAF client connections.
// Under normal operation exactly one FAF client connects; the limit guards
// against runaway reconnect loops or misconfigured launchers.
const maxRPCClients = 4

// RPCServer is a TCP server that accepts JSON-RPC connections from the FAF client.
type RPCServer struct {
	listener      net.Listener
	handler       *RPCHandler
	logger        *slog.Logger
	activeClients atomic.Int32

	clientsMu sync.Mutex
	clients   []*protocol.JSONRPCWriter
}

// NewRPCServer creates a new RPC server listening on the specified port.
func NewRPCServer(port int, handler *RPCHandler, logger *slog.Logger) (*RPCServer, error) {
	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return nil, err
	}

	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}

	return &RPCServer{
		listener: listener,
		handler:  handler,
		logger:   logger,
	}, nil
}

// Port returns the TCP port the server is listening on.
func (s *RPCServer) Port() int {
	addr, ok := s.listener.Addr().(*net.TCPAddr)
	if !ok {
		return 0
	}
	return addr.Port
}

// Run accepts client connections. Blocks until context is cancelled.
func (s *RPCServer) Run(ctx context.Context) {
	go func() {
		<-ctx.Done()
		s.listener.Close()
	}()

	for {
		conn, err := s.listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			s.logger.Error("RPC accept error", "error", err)
			continue
		}

		// Client limit: one FAF client is expected; cap at maxRPCClients.
		if s.activeClients.Load() >= maxRPCClients {
			s.logger.Warn("RPC client limit reached, rejecting",
				"limit", maxRPCClients, "remote", conn.RemoteAddr())
			conn.Close()
			continue
		}
		s.activeClients.Add(1)
		go func() {
			defer s.activeClients.Add(-1)
			s.handleConnection(ctx, conn)
		}()
	}
}

func (s *RPCServer) handleConnection(ctx context.Context, conn net.Conn) {
	defer conn.Close()

	s.logger.Info("RPC client connected", "remote", conn.RemoteAddr())

	reader := protocol.NewJSONRPCReader(conn)
	writer := protocol.NewJSONRPCWriter(conn)

	// Register client for notifications
	s.clientsMu.Lock()
	s.clients = append(s.clients, writer)
	s.clientsMu.Unlock()

	defer func() {
		s.clientsMu.Lock()
		for i, c := range s.clients {
			if c == writer {
				s.clients = append(s.clients[:i], s.clients[i+1:]...)
				break
			}
		}
		s.clientsMu.Unlock()
	}()

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		req, err := reader.ReadRequest()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			s.logger.Info("RPC client disconnected", "error", err)
			return
		}

		result, handleErr := s.handler.HandleRequest(req)
		if handleErr != nil {
			s.logger.Error("RPC handler error", "method", req.Method, "error", handleErr)
			if req.ID != nil {
				writer.WriteError(req.ID, -32603, handleErr.Error())
			}
			continue
		}

		if req.ID != nil {
			if err := writer.WriteResponse(req.ID, result); err != nil {
				s.logger.Error("RPC write response error", "error", err)
				return
			}
		}
	}
}

// SendNotification sends a notification to all connected clients.
func (s *RPCServer) SendNotification(method string, params []interface{}) {
	s.clientsMu.Lock()
	clients := make([]*protocol.JSONRPCWriter, len(s.clients))
	copy(clients, s.clients)
	s.clientsMu.Unlock()

	for _, c := range clients {
		if err := c.WriteNotification(method, params); err != nil {
			s.logger.Error("Failed to send notification", "method", method, "error", err)
		}
	}
}

// Close closes the RPC server.
func (s *RPCServer) Close() error {
	return s.listener.Close()
}
