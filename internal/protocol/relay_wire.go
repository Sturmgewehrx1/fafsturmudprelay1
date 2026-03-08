package protocol

import (
	"encoding/binary"
	"errors"
)

const (
	// RelayHeaderSize is the size of the relay packet header: 4B sender + 4B dest.
	RelayHeaderSize = 8
	// MaxRelayPacket is the maximum relay packet size (header + payload).
	MaxRelayPacket = 1400
	// MaxRelayPayload is the maximum payload size.
	MaxRelayPayload = MaxRelayPacket - RelayHeaderSize
	// BroadcastDest signals that a packet should be sent to all peers.
	BroadcastDest uint32 = 0xFFFFFFFF
)

var (
	ErrPacketTooSmall = errors.New("packet too small for relay header")
	ErrPacketTooLarge = errors.New("packet exceeds maximum relay size")
)

// RelayPacket represents a decoded relay UDP packet.
type RelayPacket struct {
	SenderID uint32
	DestID   uint32
	Payload  []byte
}

// EncodeRelayPacket encodes a relay packet: [4B sender BE][4B dest BE][payload].
func EncodeRelayPacket(senderID, destID uint32, payload []byte) ([]byte, error) {
	total := RelayHeaderSize + len(payload)
	if total > MaxRelayPacket {
		return nil, ErrPacketTooLarge
	}
	buf := make([]byte, total)
	binary.BigEndian.PutUint32(buf[0:4], senderID)
	binary.BigEndian.PutUint32(buf[4:8], destID)
	copy(buf[8:], payload)
	return buf, nil
}

// EncodeRelayHeader writes the 8-byte relay header into dst (must be >= 8 bytes).
// Returns the number of bytes written (always 8). This avoids allocation.
func EncodeRelayHeader(dst []byte, senderID, destID uint32) {
	binary.BigEndian.PutUint32(dst[0:4], senderID)
	binary.BigEndian.PutUint32(dst[4:8], destID)
}

// DecodeRelayPacket decodes a relay packet from raw bytes.
func DecodeRelayPacket(data []byte) (RelayPacket, error) {
	if len(data) < RelayHeaderSize {
		return RelayPacket{}, ErrPacketTooSmall
	}
	return RelayPacket{
		SenderID: binary.BigEndian.Uint32(data[0:4]),
		DestID:   binary.BigEndian.Uint32(data[4:8]),
		Payload:  data[RelayHeaderSize:],
	}, nil
}

// DecodeRelayHeader extracts sender and dest IDs from the first 8 bytes.
// Does NOT copy the payload — caller must handle that.
func DecodeRelayHeader(data []byte) (senderID, destID uint32, err error) {
	if len(data) < RelayHeaderSize {
		return 0, 0, ErrPacketTooSmall
	}
	return binary.BigEndian.Uint32(data[0:4]), binary.BigEndian.Uint32(data[4:8]), nil
}
