package argojob

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"sync"
	"time"
)

const holdBinary = "/oberth-hold/bin/oberth-hold"
const holdState = "/oberth-hold/state"
const maxHoldFrame = 1 << 20
const holdProtocol = 2

type holdRequest struct {
	Version   int      `json:"v"`
	Op        string   `json:"op"`
	UID       string   `json:"uid"`
	TimeoutMS int64    `json:"timeout_ms,omitempty"`
	Argv      []string `json:"argv,omitempty"`
	Stage     string   `json:"stage,omitempty"`
	Digest    string   `json:"digest,omitempty"`
}
type holdFrame struct {
	Type         string    `json:"t"`
	Data         []byte    `json:"d,omitempty"`
	Code         int       `json:"code,omitempty"`
	Reason       string    `json:"reason,omitempty"`
	HeldSince    time.Time `json:"heldSince,omitzero"`
	TTL          int64     `json:"ttl,omitempty"`
	RemainingMS  int64     `json:"remaining_ms,omitempty"`
	OriginalExit int       `json:"original_exit,omitempty"`
}

func readHoldFrame(r io.Reader, v any) error {
	var size [4]byte
	if _, err := io.ReadFull(r, size[:]); err != nil {
		return err
	}
	n := binary.BigEndian.Uint32(size[:])
	if n == 0 || n > maxHoldFrame {
		return errors.New("invalid frame size")
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		return err
	}
	return json.Unmarshal(body, v)
}
func writeHoldFrame(w io.Writer, v any) error {
	body, err := json.Marshal(v)
	if err != nil {
		return err
	}
	var size [4]byte
	binary.BigEndian.PutUint32(size[:], uint32(len(body)))
	if n, e := w.Write(size[:]); e != nil {
		return e
	} else if n != len(size) {
		return io.ErrShortWrite
	}
	n, err := w.Write(body)
	if err == nil && n != len(body) {
		return io.ErrShortWrite
	}
	return err
}

type holdStream struct {
	mu    sync.Mutex
	conn  net.Conn
	now   func() time.Time
	sent  int
	final bool
}

func (s *holdStream) send(f holdFrame) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.final {
		return net.ErrClosed
	}
	if f.Type != "out" && f.Type != "err" {
		s.final = true
	}
	_ = s.conn.SetWriteDeadline(s.now().Add(time.Second))
	return writeHoldFrame(s.conn, f)
}
func (s *holdStream) output(kind string, p []byte) error {
	s.mu.Lock()
	n := min(len(p), max(0, MaxExecOutput-s.sent))
	s.sent += n
	s.mu.Unlock()
	if n == 0 {
		return nil
	}
	return s.send(holdFrame{Type: kind, Data: p[:n]})
}
func holdFrameCode(f holdFrame) int {
	switch f.Type {
	case "exit":
		return f.Code
	case "held", "rerunning", "already_rerun":
		return 0
	case "refused":
		return 120
	case "timeout":
		return 121
	case "closed":
		return 122
	case "start_failed":
		return 123
	default:
		return 124
	}
}
