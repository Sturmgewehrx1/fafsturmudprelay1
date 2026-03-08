package protocol

import (
	"bytes"
	"encoding/binary"
	"io"
	"testing"
)

func TestGPGNetRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	w := NewGPGNetWriter(&buf)
	args := []interface{}{"hello", int32(42), "world"}
	if err := w.WriteMessage("TestCmd", args); err != nil {
		t.Fatalf("WriteMessage: %v", err)
	}

	r := NewGPGNetReader(&buf)
	header, chunks, err := r.ReadMessage()
	if err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	if header != "TestCmd" {
		t.Errorf("header = %q, want %q", header, "TestCmd")
	}
	if len(chunks) != 3 {
		t.Fatalf("len(chunks) = %d, want 3", len(chunks))
	}
	if s, ok := chunks[0].(string); !ok || s != "hello" {
		t.Errorf("chunks[0] = %v (%T), want \"hello\"", chunks[0], chunks[0])
	}
	if v, ok := chunks[1].(int32); !ok || v != 42 {
		t.Errorf("chunks[1] = %v (%T), want 42", chunks[1], chunks[1])
	}
	if s, ok := chunks[2].(string); !ok || s != "world" {
		t.Errorf("chunks[2] = %v (%T), want \"world\"", chunks[2], chunks[2])
	}
}

func TestGPGNetEscapeReplacement(t *testing.T) {
	// Build a message where a string chunk contains /t and /n
	var buf bytes.Buffer
	w := NewGPGNetWriter(&buf)
	if err := w.WriteMessage("EscTest", []interface{}{"line1/tline2/nline3"}); err != nil {
		t.Fatalf("WriteMessage: %v", err)
	}

	r := NewGPGNetReader(&buf)
	_, chunks, err := r.ReadMessage()
	if err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	if len(chunks) != 1 {
		t.Fatalf("len(chunks) = %d, want 1", len(chunks))
	}
	got := chunks[0].(string)
	want := "line1\tline2\nline3"
	if got != want {
		t.Errorf("escape replacement: got %q, want %q", got, want)
	}
}

func TestGPGNetAllArgTypes(t *testing.T) {
	tests := []struct {
		name string
		arg  interface{}
		want int32
	}{
		{"int", int(100), 100},
		{"int32", int32(-1), -1},
		{"uint16", uint16(65535), 65535},
		{"uint32", uint32(0x12345678), 0x12345678},
		{"float64", float64(3.7), 3}, // truncated to int32
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			w := NewGPGNetWriter(&buf)
			if err := w.WriteMessage("T", []interface{}{tt.arg}); err != nil {
				t.Fatalf("WriteMessage: %v", err)
			}
			r := NewGPGNetReader(&buf)
			_, chunks, err := r.ReadMessage()
			if err != nil {
				t.Fatalf("ReadMessage: %v", err)
			}
			if len(chunks) != 1 {
				t.Fatalf("len(chunks) = %d, want 1", len(chunks))
			}
			// uint32 is written directly as uint32, read back as int32
			got, ok := chunks[0].(int32)
			if !ok {
				t.Fatalf("chunk type = %T, want int32", chunks[0])
			}
			if got != tt.want {
				t.Errorf("got %d, want %d", got, tt.want)
			}
		})
	}
}

func TestGPGNetCreateLobbyMessage(t *testing.T) {
	// Simulate what java-ice-adapter sends as CreateLobby
	var buf bytes.Buffer
	w := NewGPGNetWriter(&buf)
	args := []interface{}{
		int32(0),            // LobbyInitMode (normal)
		int32(12345),        // port
		"TestPlayer",        // login
		int32(999),          // player ID
		int32(1),            // natTraversalProvider (always 1)
	}
	if err := w.WriteMessage(CmdCreateLobby, args); err != nil {
		t.Fatalf("WriteMessage: %v", err)
	}

	r := NewGPGNetReader(&buf)
	header, chunks, err := r.ReadMessage()
	if err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	if header != CmdCreateLobby {
		t.Errorf("header = %q, want %q", header, CmdCreateLobby)
	}
	if len(chunks) != 5 {
		t.Fatalf("len(chunks) = %d, want 5", len(chunks))
	}
	if v := chunks[0].(int32); v != 0 {
		t.Errorf("lobbyInitMode = %d, want 0", v)
	}
	if v := chunks[1].(int32); v != 12345 {
		t.Errorf("port = %d, want 12345", v)
	}
	if v := chunks[2].(string); v != "TestPlayer" {
		t.Errorf("login = %q, want %q", v, "TestPlayer")
	}
	if v := chunks[3].(int32); v != 999 {
		t.Errorf("playerID = %d, want 999", v)
	}
	if v := chunks[4].(int32); v != 1 {
		t.Errorf("natTraversal = %d, want 1", v)
	}
}

func TestGPGNetReadStringTooLong(t *testing.T) {
	var buf bytes.Buffer
	binary.Write(&buf, binary.LittleEndian, int32(MaxStringLength+1))
	r := NewGPGNetReader(&buf)
	_, err := r.ReadString()
	if err == nil {
		t.Fatal("expected error for oversized string")
	}
}

func TestGPGNetReadStringNegativeLength(t *testing.T) {
	var buf bytes.Buffer
	binary.Write(&buf, binary.LittleEndian, int32(-1))
	r := NewGPGNetReader(&buf)
	_, err := r.ReadString()
	if err == nil {
		t.Fatal("expected error for negative string length")
	}
}

func TestGPGNetTooManyChunks(t *testing.T) {
	var buf bytes.Buffer
	binary.Write(&buf, binary.LittleEndian, int32(MaxChunkSize+1))
	r := NewGPGNetReader(&buf)
	_, err := r.ReadChunks()
	if err == nil {
		t.Fatal("expected error for too many chunks")
	}
}

func TestGPGNetReadEOF(t *testing.T) {
	r := NewGPGNetReader(bytes.NewReader(nil))
	_, _, err := r.ReadMessage()
	if err == nil {
		t.Fatal("expected error on empty reader")
	}
}

func TestGPGNetUnsupportedArgType(t *testing.T) {
	var buf bytes.Buffer
	w := NewGPGNetWriter(&buf)
	err := w.WriteMessage("T", []interface{}{true}) // bool is unsupported
	if err == nil {
		t.Fatal("expected error for unsupported arg type")
	}
}

func TestGPGNetEmptyArgs(t *testing.T) {
	var buf bytes.Buffer
	w := NewGPGNetWriter(&buf)
	if err := w.WriteMessage("Empty", nil); err != nil {
		t.Fatalf("WriteMessage: %v", err)
	}

	r := NewGPGNetReader(&buf)
	header, chunks, err := r.ReadMessage()
	if err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	if header != "Empty" {
		t.Errorf("header = %q, want %q", header, "Empty")
	}
	if len(chunks) != 0 {
		t.Errorf("len(chunks) = %d, want 0", len(chunks))
	}
}

func TestGPGNetByteCompatibility(t *testing.T) {
	// Verify exact byte layout matches java-ice-adapter
	var buf bytes.Buffer
	w := NewGPGNetWriter(&buf)
	if err := w.WriteMessage("Hi", []interface{}{int32(7), "AB"}); err != nil {
		t.Fatalf("WriteMessage: %v", err)
	}

	data := buf.Bytes()
	r := bytes.NewReader(data)

	// Header string "Hi": [02 00 00 00] [48 69]
	var strLen int32
	binary.Read(r, binary.LittleEndian, &strLen)
	if strLen != 2 {
		t.Fatalf("header string length = %d, want 2", strLen)
	}
	strBytes := make([]byte, strLen)
	io.ReadFull(r, strBytes)
	if string(strBytes) != "Hi" {
		t.Fatalf("header string = %q, want %q", string(strBytes), "Hi")
	}

	// Args count: [02 00 00 00]
	var argCount int32
	binary.Read(r, binary.LittleEndian, &argCount)
	if argCount != 2 {
		t.Fatalf("arg count = %d, want 2", argCount)
	}

	// Arg 0: type=0x00, value=7 as int32 LE
	typeByte, _ := r.ReadByte()
	if typeByte != FieldTypeInt {
		t.Fatalf("arg0 type = 0x%02x, want 0x00", typeByte)
	}
	var intVal int32
	binary.Read(r, binary.LittleEndian, &intVal)
	if intVal != 7 {
		t.Fatalf("arg0 value = %d, want 7", intVal)
	}

	// Arg 1: type=0x01, string "AB": [02 00 00 00] [41 42]
	typeByte, _ = r.ReadByte()
	if typeByte != FieldTypeString {
		t.Fatalf("arg1 type = 0x%02x, want 0x01", typeByte)
	}
	binary.Read(r, binary.LittleEndian, &strLen)
	if strLen != 2 {
		t.Fatalf("arg1 string length = %d, want 2", strLen)
	}
	strBytes = make([]byte, strLen)
	io.ReadFull(r, strBytes)
	if string(strBytes) != "AB" {
		t.Fatalf("arg1 string = %q, want %q", string(strBytes), "AB")
	}
}

func BenchmarkGPGNetRoundTrip(b *testing.B) {
	args := []interface{}{"ConnectToPeer", int32(12345), "PlayerName"}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		var buf bytes.Buffer
		w := NewGPGNetWriter(&buf)
		w.WriteMessage("ConnectToPeer", args)
		r := NewGPGNetReader(&buf)
		r.ReadMessage()
	}
}
