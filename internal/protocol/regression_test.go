package protocol

import (
	"bytes"
	"strings"
	"testing"
)

// TestWriteStringBoundsCheck verifies strings > MaxStringLength are rejected.
// Regression: Before fix, oversized strings would corrupt the wire protocol.
func TestWriteStringBoundsCheck(t *testing.T) {
	var buf bytes.Buffer
	w := NewGPGNetWriter(&buf)

	// Exactly at the limit — should succeed
	okStr := strings.Repeat("A", MaxStringLength)
	if err := w.WriteMessage("T", []interface{}{okStr}); err != nil {
		t.Fatalf("WriteMessage with MaxStringLength: %v", err)
	}

	// Over the limit — should fail
	buf.Reset()
	tooLong := strings.Repeat("A", MaxStringLength+1)
	err := w.WriteMessage("T", []interface{}{tooLong})
	if err == nil {
		t.Fatal("expected error for oversized string")
	}
	if !strings.Contains(err.Error(), "too long") {
		t.Errorf("error should mention 'too long': %v", err)
	}
}

// TestWriteReadSymmetry verifies that ReadString bounds match WriteString bounds.
func TestWriteReadSymmetry(t *testing.T) {
	var buf bytes.Buffer
	w := NewGPGNetWriter(&buf)

	s := strings.Repeat("X", MaxStringLength)
	if err := w.WriteMessage("S", []interface{}{s}); err != nil {
		t.Fatalf("WriteMessage: %v", err)
	}

	r := NewGPGNetReader(&buf)
	header, chunks, err := r.ReadMessage()
	if err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	if header != "S" {
		t.Errorf("header = %q, want %q", header, "S")
	}
	if len(chunks) != 1 {
		t.Fatalf("len(chunks) = %d, want 1", len(chunks))
	}
	if chunks[0].(string) != s {
		t.Error("round-trip of MaxStringLength string failed")
	}
}
