package mcserver

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"
)

// Minimal Source RCON client (used for player list + say). No external deps.

const (
	rconAuth          = 3
	rconExecCommand   = 2
	rconResponseValue = 0
)

type rconConn struct {
	conn net.Conn
	r    *bufio.Reader
	id   int32
}

func rconDial(ctx context.Context, addr, password string) (*rconConn, error) {
	d := net.Dialer{Timeout: 5 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	rc := &rconConn{conn: conn, r: bufio.NewReader(conn), id: 1}
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	if err := rc.send(rconAuth, password); err != nil {
		conn.Close()
		return nil, err
	}
	respID, _, err := rc.read()
	if err != nil {
		conn.Close()
		return nil, err
	}
	if respID == -1 {
		conn.Close()
		return nil, errors.New("rcon auth failed")
	}
	return rc, nil
}

func (rc *rconConn) close() { rc.conn.Close() }

func (rc *rconConn) send(typ int32, body string) error {
	id := rc.id
	rc.id++
	payload := make([]byte, 0, len(body)+14)
	buf := make([]byte, 4)
	binary.LittleEndian.PutUint32(buf, uint32(id))
	payload = append(payload, buf...)
	binary.LittleEndian.PutUint32(buf, uint32(typ))
	payload = append(payload, buf...)
	payload = append(payload, []byte(body)...)
	payload = append(payload, 0, 0) // two null terminators

	length := make([]byte, 4)
	binary.LittleEndian.PutUint32(length, uint32(len(payload)))
	if _, err := rc.conn.Write(append(length, payload...)); err != nil {
		return err
	}
	return nil
}

func (rc *rconConn) read() (id int32, body string, err error) {
	var length int32
	if err := binary.Read(rc.r, binary.LittleEndian, &length); err != nil {
		return 0, "", err
	}
	if length < 10 || length > 4096 {
		return 0, "", fmt.Errorf("rcon: bad packet length %d", length)
	}
	packet := make([]byte, length)
	if _, err := io.ReadFull(rc.r, packet); err != nil {
		return 0, "", err
	}
	id = int32(binary.LittleEndian.Uint32(packet[0:4]))
	// packet[4:8] is type; body is up to the two trailing nulls.
	body = string(packet[8 : len(packet)-2])
	return id, body, nil
}

func (rc *rconConn) exec(command string) (string, error) {
	if err := rc.send(rconExecCommand, command); err != nil {
		return "", err
	}
	_, body, err := rc.read()
	return body, err
}

// rconCommand dials, runs one command, and returns the response.
func rconCommand(ctx context.Context, addr, password, command string) (string, error) {
	rc, err := rconDial(ctx, addr, password)
	if err != nil {
		return "", err
	}
	defer rc.close()
	return rc.exec(command)
}

// parsePlayerCount extracts N from a vanilla `list` response like
// "There are 3 of a max of 20 players online: a, b, c".
func parsePlayerCount(resp string) (int, bool) {
	resp = strings.TrimSpace(resp)
	const marker = "There are "
	i := strings.Index(resp, marker)
	if i < 0 {
		return 0, false
	}
	rest := resp[i+len(marker):]
	n := 0
	found := false
	for _, r := range rest {
		if r >= '0' && r <= '9' {
			n = n*10 + int(r-'0')
			found = true
		} else if found {
			break
		}
	}
	return n, found
}
