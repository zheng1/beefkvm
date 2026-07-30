// Package apcplog writes and reads APCP session captures.
//
// A capture is two files sharing a base name:
//
//   - <name>.bin: a stream of length-prefixed records. Each record is
//     8 bytes big-endian unix microseconds, 1 byte direction (0=client→server,
//     1=server→client, 2=note), 4 bytes big-endian payload length, N bytes
//     raw payload. Records cover the full decrypted byte stream, including
//     partial frames.
//   - <name>.jsonl: one JSON object per parsed AVMP frame with type name,
//     hex payload, and the same timestamp. Human-readable spot check.
//
// The two files can diverge in count when the .bin captures a partial frame
// or when a stream is not AVMP-shaped (SessionRequest/SessionSetup). Readers
// should not zip them together.
package apcplog

import (
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Direction of a captured byte range.
type Direction byte

const (
	DirClientToServer Direction = 0
	DirServerToClient Direction = 1
	DirNote           Direction = 2
)

func (d Direction) String() string {
	switch d {
	case DirClientToServer:
		return "c2s"
	case DirServerToClient:
		return "s2c"
	case DirNote:
		return "note"
	}
	return fmt.Sprintf("dir%d", byte(d))
}

// Writer records a capture pair.
type Writer struct {
	base string
	mu   sync.Mutex
	bin  *os.File
	json *os.File
}

// NewWriter creates the .bin and .jsonl files. base is the path without
// extension: "testdata/captures/login-only".
func NewWriter(base string) (*Writer, error) {
	if err := os.MkdirAll(filepath.Dir(base), 0o755); err != nil {
		return nil, err
	}
	bin, err := os.Create(base + ".bin")
	if err != nil {
		return nil, err
	}
	js, err := os.Create(base + ".jsonl")
	if err != nil {
		bin.Close()
		return nil, err
	}
	return &Writer{base: base, bin: bin, json: js}, nil
}

// Close flushes and closes both files.
func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	err1 := w.bin.Close()
	err2 := w.json.Close()
	if err1 != nil {
		return err1
	}
	return err2
}

// Raw appends a raw byte range to the .bin file.
func (w *Writer) Raw(dir Direction, data []byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	var hdr [13]byte
	binary.BigEndian.PutUint64(hdr[0:8], uint64(time.Now().UnixMicro()))
	hdr[8] = byte(dir)
	binary.BigEndian.PutUint32(hdr[9:13], uint32(len(data)))
	if _, err := w.bin.Write(hdr[:]); err != nil {
		return err
	}
	if _, err := w.bin.Write(data); err != nil {
		return err
	}
	return nil
}

// Frame records a parsed AVMP frame to the .jsonl sidecar. TypeName is
// caller-supplied so this package does not depend on apcp.
func (w *Writer) Frame(dir Direction, msgType uint16, typeName string, payload []byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	rec := struct {
		Micros   int64     `json:"micros"`
		Dir      string    `json:"dir"`
		Type     uint16    `json:"type"`
		TypeName string    `json:"typeName"`
		Len      int       `json:"len"`
		Payload  string    `json:"payloadHex,omitempty"`
		At       time.Time `json:"at"`
	}{
		Micros:   time.Now().UnixMicro(),
		Dir:      dir.String(),
		Type:     msgType,
		TypeName: typeName,
		Len:      len(payload),
		At:       time.Now().UTC(),
	}
	// Truncate very large payloads to first 256 bytes to keep JSONL greppable.
	preview := payload
	if len(preview) > 256 {
		preview = preview[:256]
	}
	rec.Payload = hex.EncodeToString(preview)
	enc := json.NewEncoder(w.json)
	if err := enc.Encode(rec); err != nil {
		return err
	}
	return nil
}

// Note records an arbitrary annotation. Handy for marking "user pressed A now".
func (w *Writer) Note(msg string) error {
	if err := w.Raw(DirNote, []byte(msg)); err != nil {
		return err
	}
	return w.jsonNote(msg)
}

func (w *Writer) jsonNote(msg string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	rec := struct {
		Micros int64  `json:"micros"`
		Dir    string `json:"dir"`
		Note   string `json:"note"`
	}{time.Now().UnixMicro(), "note", msg}
	return json.NewEncoder(w.json).Encode(rec)
}

// TeeReader wraps r so that every read is mirrored into w with the given
// direction. Errors from the writer are discarded — the read path is the
// primary contract.
func TeeReader(w *Writer, dir Direction, r io.Reader) io.Reader {
	return &teeReader{w: w, dir: dir, r: r}
}

type teeReader struct {
	w   *Writer
	dir Direction
	r   io.Reader
}

func (t *teeReader) Read(p []byte) (int, error) {
	n, err := t.r.Read(p)
	if n > 0 {
		_ = t.w.Raw(t.dir, p[:n])
	}
	return n, err
}

// TeeWriter mirrors writes into w with the given direction.
func TeeWriter(w *Writer, dir Direction, dst io.Writer) io.Writer {
	return &teeWriter{w: w, dir: dir, dst: dst}
}

type teeWriter struct {
	w   *Writer
	dir Direction
	dst io.Writer
}

func (t *teeWriter) Write(p []byte) (int, error) {
	_ = t.w.Raw(t.dir, p)
	return t.dst.Write(p)
}
