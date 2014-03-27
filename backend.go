package main

import (
	"io"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

type Backend struct {
	sync.Mutex
	Name      string
	Addr      string
	CheckAddr string
	up        bool
	Weight    int
	Sent      int64
	Rcvd      int64
	Errors    int64
	Conns     int64
	Active    int64

	// these are loaded from the service, se a backend doesn't need to acces
	// the service struct at all.
	dialTimeout   time.Duration
	rwTimeout     time.Duration
	checkInterval time.Duration
	rise          int
	riseCount     int
	checkOK       int
	fall          int
	fallCount     int
	checkFail     int

	startCheck sync.Once
	// stop the health-check loop
	stopCheck chan interface{}
}

// The json stats we return for the backend
type BackendStat struct {
	Name      string `json:"name"`
	Addr      string `json:"address"`
	CheckAddr string `json:"check_address"`
	Up        bool   `json:"up"`
	Weight    int    `json:"weight"`
	Sent      int64  `json:"sent"`
	Rcvd      int64  `json:"received"`
	Errors    int64  `json:"errors"`
	Conns     int64  `json:"connections"`
	Active    int64  `json:"active"`
	CheckOK   int    `json:"check_success"`
	CheckFail int    `json:"check_fail"`
}

// The subset of fields we load and serialize for config.
type BackendConfig struct {
	Name      string `json:"name"`
	Addr      string `json:"address"`
	CheckAddr string `json:"check_address"`
	Weight    int    `json:"weight"`
}

func NewBackend(cfg BackendConfig) *Backend {
	b := &Backend{
		Name:      cfg.Name,
		Addr:      cfg.Addr,
		CheckAddr: cfg.CheckAddr,
		Weight:    cfg.Weight,
		stopCheck: make(chan interface{}),
	}

	// don't want a weight of 0
	if b.Weight == 0 {
		b.Weight = 1
	}

	return b
}

// Copy the backend state into a BackendStat struct.
// We probably don't need atomic loads for the live stats here.
func (b *Backend) Stats() BackendStat {
	b.Lock()
	defer b.Unlock()

	stats := BackendStat{
		Name:      b.Name,
		Addr:      b.Addr,
		CheckAddr: b.CheckAddr,
		Up:        b.up,
		Weight:    b.Weight,
		Sent:      b.Sent,
		Rcvd:      b.Rcvd,
		Errors:    b.Errors,
		Conns:     b.Conns,
		Active:    b.Active,
		CheckOK:   b.checkOK,
		CheckFail: b.checkFail,
	}

	return stats
}

func (b *Backend) Up() bool {
	b.Lock()
	up := b.up
	b.Unlock()
	return up
}

// Return the struct for marshaling into a json config
func (b *Backend) Config() BackendConfig {
	b.Lock()
	defer b.Unlock()

	cfg := BackendConfig{
		Name:      b.Name,
		Addr:      b.Addr,
		CheckAddr: b.CheckAddr,
		Weight:    b.Weight,
	}

	return cfg
}

// Backends and Servers Stringify themselves directly into their config format.
func (b *Backend) String() string {
	return string(marshal(b.Config()))
}

func (b *Backend) Start() {
	go b.startCheck.Do(b.healthCheck)
}

func (b *Backend) Stop() {
	close(b.stopCheck)
}

func (b *Backend) check() {
	if b.CheckAddr == "" {
		return
	}

	up := true
	if c, e := net.DialTimeout("tcp", b.CheckAddr, b.dialTimeout); e == nil {
		c.Close()
	} else {
		up = false
	}

	b.Lock()
	defer b.Unlock()
	if up {
		b.fallCount = 0
		b.riseCount++
		b.checkOK++
		if b.riseCount >= b.rise {
			b.up = true
		}
	} else {
		b.riseCount = 0
		b.fallCount++
		b.checkFail++
		if b.fallCount >= b.fall {
			b.up = false
		}
	}
}

// Periodically check the status of this backend
func (b *Backend) healthCheck() {
	t := time.NewTicker(b.checkInterval)
	for {
		select {
		case <-b.stopCheck:
			t.Stop()
			return
		case <-t.C:
			b.check()
		}
	}
}

// use to identify embedded TCPConns
type closeReader interface {
	CloseRead() error
}

func (b *Backend) Proxy(conn net.Conn) {
	// Backend is a pointer receiver so we can get the address of the fields,
	// but all updates will be done atomically.
	// We still lock b in case of a config update while starting the Proxy.
	b.Lock()
	addr := b.Addr
	dialTimeout := b.dialTimeout
	// pointer values for atomic updates
	conns := &b.Conns
	active := &b.Active
	errorCount := &b.Errors
	bytesSent := &b.Sent
	bytesRcvd := &b.Rcvd
	b.Unlock()

	c, err := net.DialTimeout("tcp", addr, dialTimeout)
	if err != nil {
		log.Println("error connecting to backend", err)
		conn.Close()
		atomic.AddInt64(errorCount, 1)
		return
	}

	// TODO: might not be TCP? (this would panic)
	bConn := &timeoutConn{
		TCPConn:   c.(*net.TCPConn),
		rwTimeout: b.rwTimeout,
	}

	// TODO: No way to force shutdown. Do we need it, or hsould we always just
	// let a connection run out?

	atomic.AddInt64(conns, 1)
	atomic.AddInt64(active, 1)
	defer atomic.AddInt64(active, -1)

	// channels to wait on close event
	backendClosed := make(chan bool, 1)
	clientClosed := make(chan bool, 1)

	go broker(bConn, conn, clientClosed, bytesSent, errorCount)
	go broker(conn, bConn, backendClosed, bytesRcvd, errorCount)

	// wait for one half of the proxy to exit, then trigger a shutdown of the
	// other half by calling CloseRead(). This will break the read loop in the
	// broker and fully close the connection.
	var waitFor chan bool
	select {
	case <-clientClosed:
		bConn.CloseRead()
		waitFor = backendClosed
	case <-backendClosed:
		conn.(closeReader).CloseRead()
		waitFor = clientClosed
	}
	// wait for the other connection to close
	<-waitFor
}

// An io.Copy that updates the count during transfers.
func countingCopy(dst io.Writer, src io.Reader, written *int64) (err error) {
	buf := make([]byte, 32*1024)
	for {
		nr, er := src.Read(buf)
		if nr > 0 {
			nw, ew := dst.Write(buf[0:nr])
			if nw > 0 {
				atomic.AddInt64(written, int64(nw))
			}
			if ew != nil {
				err = ew
				break
			}
			if nr != nw {
				err = io.ErrShortWrite
				break
			}
		}
		if er == io.EOF {
			break
		}
		if er != nil {
			err = er
			break
		}
	}
	return err
}

// This does the actual data transfer.
// The broker only closes the Read side on error.
func broker(dst, src net.Conn, srcClosed chan bool, written, errors *int64) {
	err := countingCopy(dst, src, written)
	if err != nil {
		atomic.AddInt64(errors, 1)
		log.Printf("Copy error: %s", err)
	}
	if err := src.Close(); err != nil {
		atomic.AddInt64(errors, 1)
		log.Printf("Close error: %s", err)
	}
	srcClosed <- true
}

// A net.Conn that sets a deadline for every read or write operation.
// This will allow the server to close connections that are broken at the
// network level.
type timeoutConn struct {
	*net.TCPConn
	rwTimeout time.Duration
}

func (c *timeoutConn) Read(b []byte) (int, error) {
	if c.rwTimeout > 0 {
		err := c.TCPConn.SetReadDeadline(time.Now().Add(c.rwTimeout))
		if err != nil {
			return 0, err
		}
	}
	return c.TCPConn.Read(b)
}

func (c *timeoutConn) Write(b []byte) (int, error) {
	if c.rwTimeout > 0 {
		err := c.TCPConn.SetWriteDeadline(time.Now().Add(c.rwTimeout))
		if err != nil {
			return 0, err
		}
	}
	return c.TCPConn.Write(b)
}
