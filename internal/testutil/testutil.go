package testutil

import (
	"encoding/json"
	"fmt"
	"net"
	"time"

	"fafsturmudprelay/internal/protocol"
)

// GetFreeTCPPort finds and returns a free TCP port.
func GetFreeTCPPort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	return port, nil
}

// GetFreeUDPPort finds and returns a free UDP port.
func GetFreeUDPPort() (int, error) {
	addr, _ := net.ResolveUDPAddr("udp", "127.0.0.1:0")
	l, err := net.ListenUDP("udp", addr)
	if err != nil {
		return 0, err
	}
	port := l.LocalAddr().(*net.UDPAddr).Port
	l.Close()
	return port, nil
}

// WaitForTCPPort waits for a TCP port to become available.
func WaitForTCPPort(addr string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 50*time.Millisecond)
		if err == nil {
			conn.Close()
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return fmt.Errorf("timeout waiting for TCP port %s", addr)
}

// MockGPGNetClient connects to a GPGNet server and provides methods to
// send/receive binary GPGNet messages (simulating ForgedAlliance.exe).
type MockGPGNetClient struct {
	conn   net.Conn
	reader *protocol.GPGNetReader
	writer *protocol.GPGNetWriter
}

// NewMockGPGNetClient connects to a GPGNet server.
func NewMockGPGNetClient(addr string) (*MockGPGNetClient, error) {
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		return nil, err
	}
	return &MockGPGNetClient{
		conn:   conn,
		reader: protocol.NewGPGNetReader(conn),
		writer: protocol.NewGPGNetWriter(conn),
	}, nil
}

// SendMessage sends a GPGNet message.
func (m *MockGPGNetClient) SendMessage(header string, args []interface{}) error {
	return m.writer.WriteMessage(header, args)
}

// ReadMessage reads a GPGNet message with a timeout.
func (m *MockGPGNetClient) ReadMessage(timeout time.Duration) (string, []interface{}, error) {
	m.conn.SetReadDeadline(time.Now().Add(timeout))
	return m.reader.ReadMessage()
}

// Close closes the connection.
func (m *MockGPGNetClient) Close() error {
	return m.conn.Close()
}

// MockJSONRPCClient connects to a JSON-RPC server and provides methods
// to send requests and read responses (simulating the FAF client).
type MockJSONRPCClient struct {
	conn   net.Conn
	reader *protocol.JSONRPCReader
	writer *protocol.JSONRPCWriter
	nextID int
}

// NewMockJSONRPCClient connects to a JSON-RPC server.
func NewMockJSONRPCClient(addr string) (*MockJSONRPCClient, error) {
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		return nil, err
	}
	return &MockJSONRPCClient{
		conn:   conn,
		reader: protocol.NewJSONRPCReader(conn),
		writer: protocol.NewJSONRPCWriter(conn),
		nextID: 1,
	}, nil
}

// Call sends a JSON-RPC request and reads the response.
func (m *MockJSONRPCClient) Call(method string, params []interface{}) (interface{}, error) {
	id := m.nextID
	m.nextID++

	// Build request
	req := map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
		"params":  params,
	}
	data, _ := json.Marshal(req)
	m.conn.Write(data)
	m.conn.Write([]byte("\n"))

	// Read response
	m.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	raw, err := m.reader.ReadRaw()
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}

	var resp protocol.JSONRPCResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("parsing response: %w", err)
	}
	return resp.Result, nil
}

// ReadNotification reads a single notification from the server.
func (m *MockJSONRPCClient) ReadNotification(timeout time.Duration) (*protocol.JSONRPCRequest, error) {
	m.conn.SetReadDeadline(time.Now().Add(timeout))
	return m.reader.ReadRequest()
}

// Close closes the connection.
func (m *MockJSONRPCClient) Close() error {
	return m.conn.Close()
}
