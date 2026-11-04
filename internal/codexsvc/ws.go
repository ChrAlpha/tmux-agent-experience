package codexsvc

import (
	"bufio"
	"crypto/rand"
	"crypto/sha1" //nolint:gosec // RFC 6455 fixes SHA-1 for the accept key.
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// The control socket speaks WebSocket (RFC 6455): an HTTP Upgrade, then one
// JSON-RPC message per text frame. Only what that needs is here.

// Frame opcodes.
const (
	OpContinuation = 0x0
	OpText         = 0x1
	OpClose        = 0x8
	OpPing         = 0x9
	OpPong         = 0xa
)

// maxMessage bounds one message; a thread's items can be large.
const maxMessage = 16 << 20

const acceptGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// AcceptKey is the Sec-WebSocket-Accept value for a Sec-WebSocket-Key.
func AcceptKey(key string) string {
	h := sha1.Sum([]byte(key + acceptGUID)) //nolint:gosec // see import
	return base64.StdEncoding.EncodeToString(h[:])
}

// handshake upgrades the connection as a client.
func handshake(rw io.ReadWriter, r *bufio.Reader) error {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}
	key := base64.StdEncoding.EncodeToString(nonce[:])
	req := "GET /rpc HTTP/1.1\r\nHost: localhost\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n" +
		"Sec-WebSocket-Key: " + key + "\r\nSec-WebSocket-Version: 13\r\n\r\n"
	if _, err := io.WriteString(rw, req); err != nil {
		return err
	}
	resp, err := http.ReadResponse(r, nil)
	if err != nil {
		return fmt.Errorf("websocket handshake: %w", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSwitchingProtocols ||
		!strings.EqualFold(resp.Header.Get("Upgrade"), "websocket") ||
		resp.Header.Get("Sec-WebSocket-Accept") != AcceptKey(key) {
		return fmt.Errorf("websocket handshake refused: %s", resp.Status)
	}
	return nil
}

// WriteFrame writes one final frame. Clients mask what they send; servers
// must not.
func WriteFrame(w io.Writer, op byte, payload []byte, mask bool) error {
	head := []byte{0x80 | op, 0}
	n := len(payload)
	switch {
	case n < 126:
		head[1] = byte(n)
	case n <= 0xffff:
		head[1] = 126
		head = binary.BigEndian.AppendUint16(head, uint16(n))
	default:
		head[1] = 127
		head = binary.BigEndian.AppendUint64(head, uint64(n))
	}
	body := payload
	if mask {
		head[1] |= 0x80
		var key [4]byte
		if _, err := rand.Read(key[:]); err != nil {
			return err
		}
		head = append(head, key[:]...)
		body = make([]byte, n)
		for i := range payload {
			body[i] = payload[i] ^ key[i%4]
		}
	}
	if _, err := w.Write(append(head, body...)); err != nil {
		return err
	}
	return nil
}

// ReadFrame reads one frame and unmasks it if needed.
func ReadFrame(r *bufio.Reader) (op byte, fin bool, payload []byte, err error) {
	var head [2]byte
	if _, err = io.ReadFull(r, head[:]); err != nil {
		return 0, false, nil, err
	}
	fin, op = head[0]&0x80 != 0, head[0]&0x0f
	masked := head[1]&0x80 != 0
	n := uint64(head[1] & 0x7f)
	switch n {
	case 126:
		var ext [2]byte
		if _, err = io.ReadFull(r, ext[:]); err != nil {
			return 0, false, nil, err
		}
		n = uint64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err = io.ReadFull(r, ext[:]); err != nil {
			return 0, false, nil, err
		}
		n = binary.BigEndian.Uint64(ext[:])
	}
	if n > maxMessage {
		return 0, false, nil, errors.New("websocket frame too large")
	}
	var key [4]byte
	if masked {
		if _, err = io.ReadFull(r, key[:]); err != nil {
			return 0, false, nil, err
		}
	}
	payload = make([]byte, n)
	if _, err = io.ReadFull(r, payload); err != nil {
		return 0, false, nil, err
	}
	if masked {
		for i := range payload {
			payload[i] ^= key[i%4]
		}
	}
	return op, fin, payload, nil
}

// ReadMessage reads one whole message, answering pings on the way. A close
// frame ends the connection with io.EOF.
func ReadMessage(r *bufio.Reader, w io.Writer, mask bool) ([]byte, error) {
	var msg []byte
	for {
		op, fin, payload, err := ReadFrame(r)
		if err != nil {
			return nil, err
		}
		switch op {
		case OpPing:
			if err := WriteFrame(w, OpPong, payload, mask); err != nil {
				return nil, err
			}
			continue
		case OpPong:
			continue
		case OpClose:
			return nil, io.EOF
		}
		if len(msg)+len(payload) > maxMessage {
			return nil, errors.New("websocket message too large")
		}
		msg = append(msg, payload...)
		if fin {
			return msg, nil
		}
	}
}
