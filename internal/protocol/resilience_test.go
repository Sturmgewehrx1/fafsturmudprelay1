package protocol

import (
	"bytes"
	"strings"
	"testing"
)

// TestJSONRPCReadRawSizeLimit verifies that ReadRaw rejects messages
// exceeding MaxJSONRPCMessageSize.
// Regression: Before fix, a huge message would grow the buffer until OOM.
func TestJSONRPCReadRawSizeLimit(t *testing.T) {
	// Create a message with deeply nested objects that exceeds 1MB
	// Each "{" adds to depth but also to buffer size
	size := MaxJSONRPCMessageSize + 100
	var buf bytes.Buffer
	buf.WriteString("{")
	buf.WriteString(`"data":"`)
	buf.WriteString(strings.Repeat("x", size))
	buf.WriteString(`"}`)

	reader := NewJSONRPCReader(&buf)
	_, err := reader.ReadRaw()
	if err == nil {
		t.Fatal("expected error for oversized JSON-RPC message")
	}
	if !strings.Contains(err.Error(), "exceeds max size") {
		t.Errorf("expected 'exceeds max size' error, got: %v", err)
	}
}

// TestJSONRPCReadRawNormalMessage verifies that normal-sized messages
// still work after adding the size limit.
func TestJSONRPCReadRawNormalMessage(t *testing.T) {
	msg := `{"jsonrpc":"2.0","id":1,"method":"status","params":[]}`
	reader := NewJSONRPCReader(strings.NewReader(msg))
	raw, err := reader.ReadRaw()
	if err != nil {
		t.Fatalf("ReadRaw: %v", err)
	}
	if string(raw) != msg {
		t.Errorf("got %q, want %q", string(raw), msg)
	}
}

// TestJSONRPCMaxSizeConstant verifies the max size constant is reasonable.
func TestJSONRPCMaxSizeConstant(t *testing.T) {
	if MaxJSONRPCMessageSize < 1024 {
		t.Errorf("MaxJSONRPCMessageSize too small: %d", MaxJSONRPCMessageSize)
	}
	if MaxJSONRPCMessageSize > 100*1024*1024 {
		t.Errorf("MaxJSONRPCMessageSize too large: %d", MaxJSONRPCMessageSize)
	}
}
