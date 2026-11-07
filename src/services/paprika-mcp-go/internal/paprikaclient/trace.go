package paprikaclient

import (
	"crypto/tls"
	"net"
	"net/http/httptrace"
	"sync"
	"time"
)

// maxTrackedConns bounds the per-connection request counters. Requests run one
// at a time, so only a few connections are ever live; when the bound is hit the
// counters start over, which only resets the counts.
const maxTrackedConns = 64

// connCounts counts the requests made on each connection.
type connCounts struct {
	mu     sync.Mutex
	counts map[net.Conn]int
}

func newConnCounts() *connCounts {
	return &connCounts{counts: make(map[net.Conn]int)}
}

// next returns how many requests, this one included, the connection has carried.
func (c *connCounts) next(conn net.Conn) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.counts[conn]; !ok && len(c.counts) >= maxTrackedConns {
		clear(c.counts)
	}
	c.counts[conn]++
	return c.counts[conn]
}

// connTrace records what happened to the connection one request used. It adds
// no requests and reads nothing from headers or bodies.
type connTrace struct {
	mu       sync.Mutex
	got      bool
	reused   bool
	wasIdle  bool
	idleTime time.Duration
	requests int
	proto    string
}

// clientTrace returns the hooks that fill t, counting through counts.
func (t *connTrace) clientTrace(counts *connCounts) *httptrace.ClientTrace {
	return &httptrace.ClientTrace{
		GotConn: func(info httptrace.GotConnInfo) {
			n := counts.next(info.Conn)
			t.mu.Lock()
			defer t.mu.Unlock()
			t.got = true
			t.reused = info.Reused
			t.wasIdle = info.WasIdle
			t.idleTime = info.IdleTime
			t.requests = n
			t.proto = negotiatedProto(info.Conn)
		},
	}
}

// negotiatedProto names the protocol a TLS connection agreed on, in the form
// http.Response.Proto uses, or "" when it is not known yet.
func negotiatedProto(conn net.Conn) string {
	tc, ok := conn.(*tls.Conn)
	if !ok {
		return ""
	}
	switch tc.ConnectionState().NegotiatedProtocol {
	case "h2":
		return "HTTP/2.0"
	case "", "http/1.1":
		return "HTTP/1.1"
	}
	return ""
}

// setProto replaces the protocol with the one the response reported.
func (t *connTrace) setProto(proto string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.proto = proto
}

// attrs returns log attributes: reused, was_idle, idle_time, conn_requests, and
// proto. Before a connection was obtained they are left out.
func (t *connTrace) attrs() []any {
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []any
	if t.got {
		out = append(
			out,
			"reused",
			t.reused,
			"was_idle",
			t.wasIdle,
			"idle_time",
			t.idleTime,
			"conn_requests",
			t.requests,
		)
	}
	if t.proto != "" {
		out = append(out, "proto", t.proto)
	}
	return out
}
