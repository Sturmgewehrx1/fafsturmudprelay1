package protocol

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"sync"
)

// JSON-RPC 2.0 message types matching JJsonRpc library framing.
// Reading: brace-counting, byte-at-a-time, NO string awareness.
// Writing: JSON + newline + flush, mutex-protected.

// JSONRPCRequest is an incoming JSON-RPC 2.0 request (has "id").
type JSONRPCRequest struct {
	JSONRPC string            `json:"jsonrpc"`
	ID      interface{}       `json:"id"`
	Method  string            `json:"method"`
	Params  []json.RawMessage `json:"params"`
}

// JSONRPCNotification is a JSON-RPC 2.0 notification (no "id").
type JSONRPCNotification struct {
	JSONRPC string        `json:"jsonrpc"`
	Method  string        `json:"method"`
	Params  []interface{} `json:"params"`
}

// JSONRPCResponse is an outgoing JSON-RPC 2.0 success response.
type JSONRPCResponse struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      interface{} `json:"id"`
	Result  interface{} `json:"result"`
}

// JSONRPCError is an outgoing JSON-RPC 2.0 error response.
type JSONRPCError struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      interface{} `json:"id"`
	Error   *RPCError   `json:"error"`
}

// RPCError describes a JSON-RPC error.
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// MaxJSONRPCMessageSize is the maximum size of a single JSON-RPC message (1 MB).
// Prevents OOM from malicious/broken clients sending huge messages.
const MaxJSONRPCMessageSize = 1 << 20

// JSONRPCReader reads JSON-RPC messages using brace-counting.
// This exactly matches JJsonPeer.java's reading behavior:
// - Reads byte-at-a-time
// - '{' increments depth, '}' decrements
// - When depth returns to 0, a complete message is extracted
// - NO awareness of '{'/'}' inside JSON string values
type JSONRPCReader struct {
	r *bufio.Reader
}

// NewJSONRPCReader creates a new JSON-RPC reader.
func NewJSONRPCReader(r io.Reader) *JSONRPCReader {
	br, ok := r.(*bufio.Reader)
	if !ok {
		br = bufio.NewReader(r)
	}
	return &JSONRPCReader{r: br}
}

// ReadRaw reads one complete JSON object using brace-counting.
// Returns the raw bytes of the JSON object.
// Returns an error if the message exceeds MaxJSONRPCMessageSize.
func (j *JSONRPCReader) ReadRaw() ([]byte, error) {
	var buf []byte
	depth := 0
	started := false

	for {
		b, err := j.r.ReadByte()
		if err != nil {
			return nil, err
		}

		if b == '{' {
			depth++
			started = true
		} else if b == '}' {
			if started {
				depth--
			}
		}

		if started {
			buf = append(buf, b)
			if len(buf) > MaxJSONRPCMessageSize {
				return nil, fmt.Errorf("JSON-RPC message exceeds max size (%d bytes)", MaxJSONRPCMessageSize)
			}
		}

		if started && depth == 0 {
			return buf, nil
		}
	}
}

// ReadRequest reads and parses a JSON-RPC request.
// Returns the raw JSON and parsed request.
func (j *JSONRPCReader) ReadRequest() (*JSONRPCRequest, error) {
	raw, err := j.ReadRaw()
	if err != nil {
		return nil, err
	}

	// Try to parse as a request (has "id" field)
	var req JSONRPCRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, fmt.Errorf("parsing JSON-RPC message: %w", err)
	}
	return &req, nil
}

// JSONRPCWriter writes JSON-RPC messages to a stream.
// Thread-safe via mutex. Matches JJsonPeer's println + synchronized behavior.
type JSONRPCWriter struct {
	w  *bufio.Writer
	mu sync.Mutex
}

// NewJSONRPCWriter creates a new JSON-RPC writer.
func NewJSONRPCWriter(w io.Writer) *JSONRPCWriter {
	bw, ok := w.(*bufio.Writer)
	if !ok {
		bw = bufio.NewWriter(w)
	}
	return &JSONRPCWriter{w: bw}
}

// WriteResponse writes a JSON-RPC success response.
func (j *JSONRPCWriter) WriteResponse(id interface{}, result interface{}) error {
	resp := JSONRPCResponse{
		JSONRPC: "2.0",
		ID:      id,
		Result:  result,
	}
	return j.writeJSON(resp)
}

// WriteError writes a JSON-RPC error response.
func (j *JSONRPCWriter) WriteError(id interface{}, code int, message string) error {
	resp := JSONRPCError{
		JSONRPC: "2.0",
		ID:      id,
		Error:   &RPCError{Code: code, Message: message},
	}
	return j.writeJSON(resp)
}

// WriteNotification writes a JSON-RPC notification (no id).
func (j *JSONRPCWriter) WriteNotification(method string, params []interface{}) error {
	notif := JSONRPCNotification{
		JSONRPC: "2.0",
		Method:  method,
		Params:  params,
	}
	return j.writeJSON(notif)
}

// writeJSON marshals and writes a JSON object with trailing newline.
func (j *JSONRPCWriter) writeJSON(v interface{}) error {
	j.mu.Lock()
	defer j.mu.Unlock()

	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshaling JSON-RPC: %w", err)
	}
	if _, err := j.w.Write(data); err != nil {
		return err
	}
	if err := j.w.WriteByte('\n'); err != nil {
		return err
	}
	return j.w.Flush()
}
