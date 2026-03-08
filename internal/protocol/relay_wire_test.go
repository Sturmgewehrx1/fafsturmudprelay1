package protocol

import (
	"testing"
)

func TestRelayEncodeDecodeRoundTrip(t *testing.T) {
	payload := []byte("hello world")
	encoded, err := EncodeRelayPacket(1234, 5678, payload)
	if err != nil {
		t.Fatalf("EncodeRelayPacket: %v", err)
	}

	pkt, err := DecodeRelayPacket(encoded)
	if err != nil {
		t.Fatalf("DecodeRelayPacket: %v", err)
	}

	if pkt.SenderID != 1234 {
		t.Errorf("SenderID = %d, want 1234", pkt.SenderID)
	}
	if pkt.DestID != 5678 {
		t.Errorf("DestID = %d, want 5678", pkt.DestID)
	}
	if string(pkt.Payload) != "hello world" {
		t.Errorf("Payload = %q, want %q", pkt.Payload, "hello world")
	}
}

func TestRelayBroadcast(t *testing.T) {
	payload := []byte{0x01, 0x02, 0x03}
	encoded, err := EncodeRelayPacket(42, BroadcastDest, payload)
	if err != nil {
		t.Fatalf("EncodeRelayPacket: %v", err)
	}

	pkt, err := DecodeRelayPacket(encoded)
	if err != nil {
		t.Fatalf("DecodeRelayPacket: %v", err)
	}

	if pkt.DestID != BroadcastDest {
		t.Errorf("DestID = 0x%08x, want 0x%08x", pkt.DestID, BroadcastDest)
	}
}

func TestRelayHeaderEncoding(t *testing.T) {
	// Verify Big-Endian byte order (4 bytes per ID)
	encoded, _ := EncodeRelayPacket(0x00010203, 0x04050607, []byte{0xFF})
	if encoded[0] != 0x00 || encoded[1] != 0x01 || encoded[2] != 0x02 || encoded[3] != 0x03 {
		t.Errorf("senderID bytes = [%02x %02x %02x %02x], want [00 01 02 03]", encoded[0], encoded[1], encoded[2], encoded[3])
	}
	if encoded[4] != 0x04 || encoded[5] != 0x05 || encoded[6] != 0x06 || encoded[7] != 0x07 {
		t.Errorf("destID bytes = [%02x %02x %02x %02x], want [04 05 06 07]", encoded[4], encoded[5], encoded[6], encoded[7])
	}
	if encoded[8] != 0xFF {
		t.Errorf("payload byte = %02x, want FF", encoded[8])
	}
}

func TestRelayDecodeHeaderOnly(t *testing.T) {
	data := []byte{0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x02}
	sender, dest, err := DecodeRelayHeader(data)
	if err != nil {
		t.Fatalf("DecodeRelayHeader: %v", err)
	}
	if sender != 1 {
		t.Errorf("sender = %d, want 1", sender)
	}
	if dest != 2 {
		t.Errorf("dest = %d, want 2", dest)
	}
}

func TestRelayEncodeHeaderInPlace(t *testing.T) {
	buf := make([]byte, 10)
	EncodeRelayHeader(buf, 0x00001234, 0x00005678)
	if buf[0] != 0x00 || buf[1] != 0x00 || buf[2] != 0x12 || buf[3] != 0x34 {
		t.Errorf("sender bytes = [%02x %02x %02x %02x], want [00 00 12 34]", buf[0], buf[1], buf[2], buf[3])
	}
	if buf[4] != 0x00 || buf[5] != 0x00 || buf[6] != 0x56 || buf[7] != 0x78 {
		t.Errorf("dest bytes = [%02x %02x %02x %02x], want [00 00 56 78]", buf[4], buf[5], buf[6], buf[7])
	}
}

func TestRelayPacketTooSmall(t *testing.T) {
	_, err := DecodeRelayPacket([]byte{0x01, 0x02})
	if err != ErrPacketTooSmall {
		t.Errorf("err = %v, want ErrPacketTooSmall", err)
	}

	_, _, err = DecodeRelayHeader([]byte{0x01})
	if err != ErrPacketTooSmall {
		t.Errorf("err = %v, want ErrPacketTooSmall", err)
	}
}

func TestRelayPacketTooLarge(t *testing.T) {
	payload := make([]byte, MaxRelayPayload+1)
	_, err := EncodeRelayPacket(1, 2, payload)
	if err != ErrPacketTooLarge {
		t.Errorf("err = %v, want ErrPacketTooLarge", err)
	}
}

func TestRelayEmptyPayload(t *testing.T) {
	encoded, err := EncodeRelayPacket(1, 2, nil)
	if err != nil {
		t.Fatalf("EncodeRelayPacket: %v", err)
	}
	if len(encoded) != RelayHeaderSize {
		t.Errorf("len = %d, want %d", len(encoded), RelayHeaderSize)
	}

	pkt, err := DecodeRelayPacket(encoded)
	if err != nil {
		t.Fatalf("DecodeRelayPacket: %v", err)
	}
	if len(pkt.Payload) != 0 {
		t.Errorf("payload len = %d, want 0", len(pkt.Payload))
	}
}

func TestRelayMaxPayload(t *testing.T) {
	payload := make([]byte, MaxRelayPayload)
	encoded, err := EncodeRelayPacket(1, 2, payload)
	if err != nil {
		t.Fatalf("EncodeRelayPacket: %v", err)
	}
	if len(encoded) != MaxRelayPacket {
		t.Errorf("len = %d, want %d", len(encoded), MaxRelayPacket)
	}
}

func BenchmarkRelayEncodeDecode(b *testing.B) {
	payload := make([]byte, 500) // typical game packet
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		encoded, _ := EncodeRelayPacket(1234, 5678, payload)
		DecodeRelayPacket(encoded)
	}
}
