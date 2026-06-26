package server

// Minimal RFC 6455 server-side WebSocket built on the standard library only —
// no gorilla/nhooyr dependency. Handles the upgrade handshake, client->server
// frame unmasking, fragmentation (continuation frames), and ping/pong/close
// control frames. Enough to run the collaborative editor; not a full library.

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
)

const wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// Conn is a single WebSocket connection.
type Conn struct {
	conn net.Conn
	br   *bufio.Reader
	bw   *bufio.Writer
	wmu  sync.Mutex
}

// Upgrade performs the WebSocket handshake by hijacking the HTTP connection.
func Upgrade(w http.ResponseWriter, r *http.Request) (*Conn, error) {
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		return nil, errors.New("not a websocket upgrade")
	}
	key := r.Header.Get("Sec-WebSocket-Key")
	if key == "" {
		return nil, errors.New("missing Sec-WebSocket-Key")
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		return nil, errors.New("response writer does not support hijack")
	}
	conn, rw, err := hj.Hijack()
	if err != nil {
		return nil, err
	}
	sum := sha1.Sum([]byte(key + wsGUID))
	accept := base64.StdEncoding.EncodeToString(sum[:])
	_, err = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + accept + "\r\n\r\n")
	if err == nil {
		err = rw.Flush()
	}
	if err != nil {
		conn.Close()
		return nil, err
	}
	return &Conn{conn: conn, br: rw.Reader, bw: rw.Writer}, nil
}

func (c *Conn) readFrame() (fin bool, opcode byte, payload []byte, err error) {
	var h [2]byte
	if _, err = io.ReadFull(c.br, h[:]); err != nil {
		return
	}
	fin = h[0]&0x80 != 0
	opcode = h[0] & 0x0f
	masked := h[1]&0x80 != 0
	length := uint64(h[1] & 0x7f)
	switch length {
	case 126:
		var ext [2]byte
		if _, err = io.ReadFull(c.br, ext[:]); err != nil {
			return
		}
		length = uint64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err = io.ReadFull(c.br, ext[:]); err != nil {
			return
		}
		length = binary.BigEndian.Uint64(ext[:])
	}
	var mask [4]byte
	if masked {
		if _, err = io.ReadFull(c.br, mask[:]); err != nil {
			return
		}
	}
	payload = make([]byte, length)
	if _, err = io.ReadFull(c.br, payload); err != nil {
		return
	}
	if masked {
		for i := range payload {
			payload[i] ^= mask[i%4]
		}
	}
	return
}

// ReadMessage returns the next complete text/binary message, reassembling
// fragments and transparently answering pings.
func (c *Conn) ReadMessage() (string, error) {
	var data []byte
	for {
		fin, opcode, payload, err := c.readFrame()
		if err != nil {
			return "", err
		}
		switch opcode {
		case 0x0, 0x1, 0x2: // continuation, text, binary
			data = append(data, payload...)
			if fin {
				return string(data), nil
			}
		case 0x8: // close
			_ = c.writeFrame(0x8, nil)
			return "", io.EOF
		case 0x9: // ping -> pong
			_ = c.writeFrame(0xA, payload)
		case 0xA: // pong, ignore
		}
	}
}

func (c *Conn) writeFrame(opcode byte, payload []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	b0 := byte(0x80) | opcode
	n := len(payload)
	var head []byte
	switch {
	case n < 126:
		head = []byte{b0, byte(n)}
	case n < 65536:
		head = []byte{b0, 126, byte(n >> 8), byte(n)}
	default:
		head = make([]byte, 10)
		head[0], head[1] = b0, 127
		binary.BigEndian.PutUint64(head[2:], uint64(n))
	}
	if _, err := c.bw.Write(head); err != nil {
		return err
	}
	if _, err := c.bw.Write(payload); err != nil {
		return err
	}
	return c.bw.Flush()
}

// WriteMessage sends a text frame.
func (c *Conn) WriteMessage(s string) error { return c.writeFrame(0x1, []byte(s)) }

// Close closes the underlying TCP connection.
func (c *Conn) Close() error { return c.conn.Close() }
