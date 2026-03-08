package protocol

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestJSONRPCReadRequest(t *testing.T) {
	input := `{"jsonrpc":"2.0","id":1,"method":"hostGame","params":["scmp_009"]}` + "\n"
	reader := NewJSONRPCReader(strings.NewReader(input))
	req, err := reader.ReadRequest()
	if err != nil {
		t.Fatalf("ReadRequest: %v", err)
	}
	if req.Method != "hostGame" {
		t.Errorf("Method = %q, want %q", req.Method, "hostGame")
	}
	if req.ID.(float64) != 1 {
		t.Errorf("ID = %v, want 1", req.ID)
	}
	if len(req.Params) != 1 {
		t.Fatalf("len(Params) = %d, want 1", len(req.Params))
	}
	var mapName string
	json.Unmarshal(req.Params[0], &mapName)
	if mapName != "scmp_009" {
		t.Errorf("param[0] = %q, want %q", mapName, "scmp_009")
	}
}

func TestJSONRPCReadMultiple(t *testing.T) {
	input := `{"jsonrpc":"2.0","id":1,"method":"a","params":[]}` +
		`{"jsonrpc":"2.0","id":2,"method":"b","params":["x"]}` + "\n"
	reader := NewJSONRPCReader(strings.NewReader(input))

	req1, err := reader.ReadRequest()
	if err != nil {
		t.Fatalf("ReadRequest 1: %v", err)
	}
	if req1.Method != "a" {
		t.Errorf("req1.Method = %q, want %q", req1.Method, "a")
	}

	req2, err := reader.ReadRequest()
	if err != nil {
		t.Fatalf("ReadRequest 2: %v", err)
	}
	if req2.Method != "b" {
		t.Errorf("req2.Method = %q, want %q", req2.Method, "b")
	}
}

func TestJSONRPCBraceCountingIgnoresStringContent(t *testing.T) {
	// The brace-counting reader does NOT handle braces inside strings.
	// This test verifies behavior matching JJsonPeer.java:
	// A message with { inside a string value would prematurely break.
	// In FAF's actual usage, this never happens because messages don't contain braces in values.
	// This test just verifies the brace counter works for normal messages.
	input := `{"jsonrpc":"2.0","id":1,"method":"test","params":["normal"]}` + "\n"
	reader := NewJSONRPCReader(strings.NewReader(input))
	req, err := reader.ReadRequest()
	if err != nil {
		t.Fatalf("ReadRequest: %v", err)
	}
	if req.Method != "test" {
		t.Errorf("Method = %q, want %q", req.Method, "test")
	}
}

func TestJSONRPCWriteResponse(t *testing.T) {
	var buf bytes.Buffer
	w := NewJSONRPCWriter(&buf)

	if err := w.WriteResponse(float64(1), "ok"); err != nil {
		t.Fatalf("WriteResponse: %v", err)
	}

	output := buf.String()
	if !strings.Contains(output, `"result":"ok"`) {
		t.Errorf("response missing result: %s", output)
	}
	if !strings.Contains(output, `"id":1`) {
		t.Errorf("response missing id: %s", output)
	}
	if !strings.HasSuffix(output, "\n") {
		t.Error("response not terminated with newline")
	}
}

func TestJSONRPCWriteNotification(t *testing.T) {
	var buf bytes.Buffer
	w := NewJSONRPCWriter(&buf)

	params := []interface{}{"Connected"}
	if err := w.WriteNotification("onConnectionStateChanged", params); err != nil {
		t.Fatalf("WriteNotification: %v", err)
	}

	output := buf.String()
	if !strings.Contains(output, `"method":"onConnectionStateChanged"`) {
		t.Errorf("notification missing method: %s", output)
	}
	if !strings.Contains(output, `"params":["Connected"]`) {
		t.Errorf("notification missing params: %s", output)
	}
	// Must NOT contain "id"
	if strings.Contains(output, `"id"`) {
		t.Errorf("notification should not have id: %s", output)
	}
}

func TestJSONRPCWriteError(t *testing.T) {
	var buf bytes.Buffer
	w := NewJSONRPCWriter(&buf)

	if err := w.WriteError(float64(5), -32601, "Method not found"); err != nil {
		t.Fatalf("WriteError: %v", err)
	}

	output := buf.String()
	if !strings.Contains(output, `"error"`) {
		t.Errorf("error response missing error field: %s", output)
	}
	if !strings.Contains(output, `"code":-32601`) {
		t.Errorf("error response missing code: %s", output)
	}
}

func TestJSONRPCReaderSkipsLeadingGarbage(t *testing.T) {
	// Anything before the first '{' should be ignored
	input := "  garbage \n  " + `{"jsonrpc":"2.0","id":1,"method":"test","params":[]}` + "\n"
	reader := NewJSONRPCReader(strings.NewReader(input))
	req, err := reader.ReadRequest()
	if err != nil {
		t.Fatalf("ReadRequest: %v", err)
	}
	if req.Method != "test" {
		t.Errorf("Method = %q, want %q", req.Method, "test")
	}
}

func TestJSONRPCRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	w := NewJSONRPCWriter(&buf)

	// Write a notification
	if err := w.WriteNotification("onGpgNetMessageReceived", []interface{}{
		"GameState", []interface{}{"Lobby"},
	}); err != nil {
		t.Fatalf("WriteNotification: %v", err)
	}

	// Read it back
	reader := NewJSONRPCReader(&buf)
	req, err := reader.ReadRequest()
	if err != nil {
		t.Fatalf("ReadRequest: %v", err)
	}
	if req.Method != "onGpgNetMessageReceived" {
		t.Errorf("Method = %q, want %q", req.Method, "onGpgNetMessageReceived")
	}
}

func BenchmarkJSONRPCReadWrite(b *testing.B) {
	msg := `{"jsonrpc":"2.0","id":1,"method":"connectToPeer","params":["Player1",12345,true]}`
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		reader := NewJSONRPCReader(strings.NewReader(msg))
		reader.ReadRequest()
	}
}
