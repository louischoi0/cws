package main

import (
	"bufio"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"
)

// KDSClient speaks KDS's newline text protocol (manual/client/client.md):
// one command per line in, exactly one reply line back, errors are plain
// reply lines prefixed "ERR ". The server handles all connections on one
// cooperative thread, so this client serializes every statement behind a
// mutex rather than pooling connections.
type KDSClient struct {
	addr string

	mu   sync.Mutex
	conn net.Conn
	r    *bufio.Reader
}

func NewKDSClient(addr string) *KDSClient {
	return &KDSClient{addr: addr}
}

// Connect dials KDS, retrying with the given delay — useful at startup if
// kds_server is still coming up.
func (c *KDSClient) Connect(attempts int, delay time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	var err error
	for i := 0; i < attempts; i++ {
		if err = c.dialLocked(); err == nil {
			return nil
		}
		time.Sleep(delay)
	}
	return fmt.Errorf("connect to kds at %s: %w", c.addr, err)
}

func (c *KDSClient) dialLocked() error {
	conn, err := net.DialTimeout("tcp", c.addr, 5*time.Second)
	if err != nil {
		return err
	}
	c.conn = conn
	c.r = bufio.NewReader(conn)
	return nil
}

// Exec sends one statement and returns its reply line (with the trailing
// "ERR " error rendered as a Go error). On any I/O failure the connection
// is dropped rather than silently retried — a retry after a write whose
// success we couldn't confirm risks a duplicate INSERT, which is worse
// than surfacing the error to the caller.
func (c *KDSClient) Exec(stmt string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.conn == nil {
		if err := c.dialLocked(); err != nil {
			return "", fmt.Errorf("kds not connected: %w", err)
		}
	}

	reply, err := c.execLocked(stmt)
	if err != nil {
		c.dropLocked()
	}
	return reply, err
}

// ExecTxn runs stmts inside one KDS transaction (BEGIN ... COMMIT), holding
// the connection for the whole sequence. KDS statements are strictly serial
// per connection with no request ids (manual/client/client.md §2), so if
// Exec's per-statement locking were used here, another goroutine's unrelated
// statement could land between two of these and run inside our transaction
// by accident. A statement failure rolls back and keeps the connection (the
// transaction failed, not the connection); a failure to even complete
// BEGIN/ROLLBACK/COMMIT drops it, same as Exec.
func (c *KDSClient) ExecTxn(stmts []string) ([]string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.conn == nil {
		if err := c.dialLocked(); err != nil {
			return nil, fmt.Errorf("kds not connected: %w", err)
		}
	}

	if _, err := c.execLocked("BEGIN"); err != nil {
		c.dropLocked()
		return nil, err
	}

	replies := make([]string, 0, len(stmts))
	for _, stmt := range stmts {
		reply, err := c.execLocked(stmt)
		if err != nil {
			if _, rerr := c.execLocked("ROLLBACK"); rerr != nil {
				c.dropLocked()
			}
			return nil, err
		}
		replies = append(replies, reply)
	}

	if _, err := c.execLocked("COMMIT"); err != nil {
		c.dropLocked()
		return nil, err
	}
	return replies, nil
}

func (c *KDSClient) dropLocked() {
	if c.conn != nil {
		c.conn.Close()
		c.conn = nil
	}
}

func (c *KDSClient) execLocked(stmt string) (string, error) {
	if _, err := c.conn.Write([]byte(stmt + "\n")); err != nil {
		return "", err
	}
	line, err := c.r.ReadString('\n')
	if err != nil {
		return "", err
	}
	line = strings.TrimRight(line, "\r\n")
	if strings.HasPrefix(line, "ERR ") {
		return "", fmt.Errorf("%s", line)
	}
	return line, nil
}
