package runtime_test

import (
	"bytes"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// slowProxy is a TCP proxy in front of Redis that can delay the reply of the
// next command containing a marker (e.g. "XADD"): the client's read times out
// although Redis applied the command, like a fork/BGSAVE pause or a network blip.
type slowProxy struct {
	ln      net.Listener
	target  string
	marker  atomic.Pointer[[]byte]
	delay   time.Duration
	delayed atomic.Int32
	mu      sync.Mutex
	conns   []net.Conn
}

func newSlowProxy(t *testing.T, target string, delay time.Duration) *slowProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &slowProxy{ln: ln, target: target, delay: delay}
	go p.accept()
	t.Cleanup(func() {
		_ = ln.Close()
		p.mu.Lock()
		defer p.mu.Unlock()
		for _, c := range p.conns {
			_ = c.Close()
		}
	})
	return p
}

func (p *slowProxy) Addr() string { return p.ln.Addr().String() }

// delayNext delays the reply of the next command containing marker (once).
func (p *slowProxy) delayNext(marker string) { m := []byte(marker); p.marker.Store(&m) }

func (p *slowProxy) accept() {
	for {
		c, err := p.ln.Accept()
		if err != nil {
			return
		}
		s, err := net.Dial("tcp", p.target)
		if err != nil {
			_ = c.Close()
			continue
		}
		p.mu.Lock()
		p.conns = append(p.conns, c, s)
		p.mu.Unlock()
		var slow atomic.Bool
		go func() { // client -> server: spot the marked command
			buf := make([]byte, 32<<10)
			for {
				n, err := c.Read(buf)
				if n > 0 {
					if m := p.marker.Load(); m != nil && bytes.Contains(buf[:n], *m) && p.marker.CompareAndSwap(m, nil) {
						slow.Store(true)
					}
					if _, werr := s.Write(buf[:n]); werr != nil {
						return
					}
				}
				if err != nil {
					_ = s.Close()
					return
				}
			}
		}()
		go func() { // server -> client: hold the marked command's reply
			buf := make([]byte, 32<<10)
			for {
				n, err := s.Read(buf)
				if n > 0 {
					if slow.CompareAndSwap(true, false) {
						p.delayed.Add(1)
						time.Sleep(p.delay)
					}
					if _, werr := c.Write(buf[:n]); werr != nil {
						return
					}
				}
				if err != nil {
					_ = c.Close()
					return
				}
			}
		}()
	}
}
