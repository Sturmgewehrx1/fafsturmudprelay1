package protocol

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"strings"
	"sync"
)

const (
	FieldTypeInt            byte = 0x00
	FieldTypeString         byte = 0x01
	FieldTypeFollowUpString byte = 0x02

	MaxChunkSize    = 10
	MaxStringLength = 65535
)

// GPGNetReader reads GPGNet binary messages from a stream.
// Wire format verified from java-ice-adapter's FaDataInputStream.java:
// Little-Endian integers, UTF-8 strings, length-prefixed.
type GPGNetReader struct {
	r io.Reader
}

func NewGPGNetReader(r io.Reader) *GPGNetReader {
	return &GPGNetReader{r: r}
}

// ReadInt reads a little-endian int32.
func (g *GPGNetReader) ReadInt() (int32, error) {
	var v int32
	if err := binary.Read(g.r, binary.LittleEndian, &v); err != nil {
		return 0, err
	}
	return v, nil
}

// ReadString reads a length-prefixed UTF-8 string.
// Format: [int32 LE: byte count][UTF-8 bytes]
func (g *GPGNetReader) ReadString() (string, error) {
	size, err := g.ReadInt()
	if err != nil {
		return "", fmt.Errorf("reading string length: %w", err)
	}
	if size < 0 || size > MaxStringLength {
		return "", fmt.Errorf("invalid string length: %d", size)
	}
	buf := make([]byte, size)
	if _, err := io.ReadFull(g.r, buf); err != nil {
		return "", fmt.Errorf("reading string bytes: %w", err)
	}
	return string(buf), nil
}

// ReadChunks reads a chunk array from the stream.
// Format: [int32 LE: count][for each: [byte: type][typed data]]
// Type 0x00 = int32 LE, anything else = string (with escape replacement).
func (g *GPGNetReader) ReadChunks() ([]interface{}, error) {
	count, err := g.ReadInt()
	if err != nil {
		return nil, fmt.Errorf("reading chunk count: %w", err)
	}
	if count > MaxChunkSize {
		return nil, fmt.Errorf("too many chunks: %d", count)
	}

	chunks := make([]interface{}, 0, count)
	var typeBuf [1]byte

	for i := int32(0); i < count; i++ {
		if _, err := io.ReadFull(g.r, typeBuf[:]); err != nil {
			return nil, fmt.Errorf("reading chunk type: %w", err)
		}

		switch typeBuf[0] {
		case FieldTypeInt:
			v, err := g.ReadInt()
			if err != nil {
				return nil, fmt.Errorf("reading int chunk: %w", err)
			}
			chunks = append(chunks, v)
		default:
			// FieldTypeString, FieldTypeFollowUpString, or any other value → string
			s, err := g.ReadString()
			if err != nil {
				return nil, fmt.Errorf("reading string chunk: %w", err)
			}
			s = replaceEscapes(s)
			chunks = append(chunks, s)
		}
	}
	return chunks, nil
}

// ReadMessage reads a complete GPGNet message: header string + chunks.
func (g *GPGNetReader) ReadMessage() (header string, chunks []interface{}, err error) {
	header, err = g.ReadString()
	if err != nil {
		return "", nil, fmt.Errorf("reading message header: %w", err)
	}
	chunks, err = g.ReadChunks()
	if err != nil {
		return "", nil, fmt.Errorf("reading message chunks: %w", err)
	}
	return header, chunks, nil
}

// replaceEscapes replaces FA-specific escape sequences in received strings.
func replaceEscapes(s string) string {
	s = strings.ReplaceAll(s, "/t", "\t")
	s = strings.ReplaceAll(s, "/n", "\n")
	return s
}

// GPGNetWriter writes GPGNet binary messages to a stream.
// Wire format verified from java-ice-adapter's FaDataOutputStream.java.
type GPGNetWriter struct {
	w  *bufio.Writer
	mu sync.Mutex
}

func NewGPGNetWriter(w io.Writer) *GPGNetWriter {
	bw, ok := w.(*bufio.Writer)
	if !ok {
		bw = bufio.NewWriter(w)
	}
	return &GPGNetWriter{w: bw}
}

// WriteInt writes a little-endian int32.
func (g *GPGNetWriter) WriteInt(v int32) error {
	return binary.Write(g.w, binary.LittleEndian, v)
}

// WriteByte writes a single byte.
func (g *GPGNetWriter) WriteByte(b byte) error {
	return g.w.WriteByte(b)
}

// WriteString writes a length-prefixed UTF-8 string.
// Format: [int32 LE: len(s)][UTF-8 bytes]
// Note: len(s) in Go returns byte count, which matches Java's string.length()
// for ASCII strings (all FAF strings are ASCII).
func (g *GPGNetWriter) WriteString(s string) error {
	if len(s) > MaxStringLength {
		return fmt.Errorf("string too long: %d > %d", len(s), MaxStringLength)
	}
	if err := g.WriteInt(int32(len(s))); err != nil {
		return err
	}
	_, err := g.w.Write([]byte(s))
	return err
}

// WriteArgs writes a typed argument list.
// Format: [int32 LE: count][for each: [byte: type][typed data]]
func (g *GPGNetWriter) WriteArgs(args []interface{}) error {
	if err := g.WriteInt(int32(len(args))); err != nil {
		return err
	}
	for _, arg := range args {
		switch v := arg.(type) {
		case int:
			if err := g.WriteByte(FieldTypeInt); err != nil {
				return err
			}
			if err := g.WriteInt(int32(v)); err != nil {
				return err
			}
		case int32:
			if err := g.WriteByte(FieldTypeInt); err != nil {
				return err
			}
			if err := g.WriteInt(v); err != nil {
				return err
			}
		case uint16:
			if err := g.WriteByte(FieldTypeInt); err != nil {
				return err
			}
			if err := g.WriteInt(int32(v)); err != nil {
				return err
			}
		case uint32:
			if err := g.WriteByte(FieldTypeInt); err != nil {
				return err
			}
			if err := binary.Write(g.w, binary.LittleEndian, v); err != nil {
				return err
			}
		case float64:
			// JSON numbers come as float64; convert to int32 (matches Java's Double.intValue())
			if err := g.WriteByte(FieldTypeInt); err != nil {
				return err
			}
			if err := g.WriteInt(int32(v)); err != nil {
				return err
			}
		case string:
			if err := g.WriteByte(FieldTypeString); err != nil {
				return err
			}
			if err := g.WriteString(v); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unsupported arg type %T", arg)
		}
	}
	return nil
}

// WriteMessage writes a complete GPGNet message: header + args, then flushes.
// Thread-safe via mutex.
func (g *GPGNetWriter) WriteMessage(header string, args []interface{}) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	if err := g.WriteString(header); err != nil {
		return fmt.Errorf("writing message header: %w", err)
	}
	if err := g.WriteArgs(args); err != nil {
		return fmt.Errorf("writing message args: %w", err)
	}
	return g.w.Flush()
}
