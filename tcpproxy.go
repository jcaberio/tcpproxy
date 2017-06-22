// Copyright 2017 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package tcpproxy lets users build TCP, HTTP/1, and TLS+SNI proxies.
//
// Typical usage:
//
//     var p tcpproxy.Proxy
//     p.AddHTTPHostRoute(":80", "foo.com", tcpproxy.To("10.0.0.1:8081"))
//     p.AddHTTPHostRoute(":80", "bar.com", tcpproxy.To("10.0.0.2:8082"))
//     p.AddRoute(":80", tcpproxy.To("10.0.0.1:8081")) // fallback
//     p.AddSNIHostRoute(":443", "foo.com", tcpproxy.To("10.0.0.1:4431"))
//     p.AddSNIHostRoute(":443", "bar.com", tcpproxy.To("10.0.0.2:4432"))
//     p.AddRoute(":443", tcpproxy.To("10.0.0.1:4431")) // fallback
//     log.Fatal(p.Run())
package tcpproxy

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"net"
	"time"
)

// Proxy is a proxy. Its zero value is a valid proxy that does
// nothing. Call methods to add routes before calling Run.
type Proxy struct {
	routes map[string][]route // ip:port => route

	lns   []net.Listener
	donec chan struct{} // closed before err
	err   error         // any error from listening

	// ListenFunc optionally specifies an alternate listen
	// function. If nil, net.Dial is used.
	// The provided net is always "tcp".
	ListenFunc func(net, laddr string) (net.Listener, error)
}

type route struct {
	matcher matcher
	target  Target
}

type matcher interface {
	match(*bufio.Reader) bool
}

func (p *Proxy) netListen() func(net, laddr string) (net.Listener, error) {
	if p.ListenFunc != nil {
		return p.ListenFunc
	}
	return net.Listen
}

func (p *Proxy) addRoute(ipPort string, matcher matcher, dest Target) {
	if p.routes == nil {
		p.routes = make(map[string][]route)
	}
	p.routes[ipPort] = append(p.routes[ipPort], route{matcher, dest})
}

// AddRoute appends an always-matching route to the ipPort listener,
// directing any connection to dest.
//
// This is generally used as either the only rule (for simple TCP
// proxies), or as the final fallback rule for an ipPort.
//
// The ipPort is any valid net.Listen TCP address.
func (p *Proxy) AddRoute(ipPort string, dest Target) {
	p.addRoute(ipPort, alwaysMatch{}, dest)
}

type alwaysMatch struct{}

func (alwaysMatch) match(*bufio.Reader) bool { return true }

// Run is calls Start, and then Wait.
//
// It blocks until there's an error. The return value is always
// non-nil.
func (p *Proxy) Run() error {
	if err := p.Start(); err != nil {
		return err
	}
	return p.Wait()
}

// Wait waits for the Proxy to finish running. Currently this can only
// happen if a Listener is closed, or Close is called on the proxy.
//
// It is only valid to call Wait after a successful call to Start.
func (p *Proxy) Wait() error {
	<-p.donec
	return p.err
}

// Close closes all the proxy's self-opened listeners.
func (p *Proxy) Close() error {
	for _, c := range p.lns {
		c.Close()
	}
	return nil
}

// Start creates a TCP listener for each unique ipPort from the
// previously created routes and starts the proxy. It returns any
// error from starting listeners.
//
// If it returns a non-nil error, any successfully opened listeners
// are closed.
func (p *Proxy) Start() error {
	if p.donec != nil {
		return errors.New("already started")
	}
	p.donec = make(chan struct{})
	errc := make(chan error, len(p.routes))
	p.lns = make([]net.Listener, 0, len(p.routes))
	for ipPort, routes := range p.routes {
		ln, err := p.netListen()("tcp", ipPort)
		if err != nil {
			p.Close()
			return err
		}
		p.lns = append(p.lns, ln)
		go p.serveListener(errc, ln, routes)
	}
	go p.awaitFirstError(errc)
	return nil
}

func (p *Proxy) awaitFirstError(errc <-chan error) {
	p.err = <-errc
	close(p.donec)
}

func (p *Proxy) serveListener(ret chan<- error, ln net.Listener, routes []route) {
	for {
		c, err := ln.Accept()
		if err != nil {
			ret <- err
			return
		}
		go p.serveConn(c, routes)
	}
}

// serveConn runs in its own goroutine and matches c against routes.
// It returns whether it matched purely for testing.
func (p *Proxy) serveConn(c net.Conn, routes []route) bool {
	br := bufio.NewReader(c)
	for _, route := range routes {
		if route.matcher.match(br) {
			buffered, _ := br.Peek(br.Buffered())
			route.target.HandleConn(changeReaderConn{
				r:    io.MultiReader(bytes.NewReader(buffered), c),
				Conn: c,
			}, c)
			return true
		}
	}
	// TODO: hook for this?
	log.Printf("tcpproxy: no routes matched conn %v/%v; closing", c.RemoteAddr().String(), c.LocalAddr().String())
	c.Close()
	return false
}

// changeReaderConn is a net.Conn wrapper with a separate reader function.
type changeReaderConn struct {
	r io.Reader
	net.Conn
}

func (c changeReaderConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// Target is what an incoming matched connection is sent to.
type Target interface {
	// HandleConn is called when an incoming connection is
	// matched. After the call to HandleConn, the tcpproxy
	// package never touches the conn again. Implementations are
	// responsible for closing the conn when needed.
	//
	// The c Conn acts like a new conn, without any bytes consumed,
	// but it has an unexported concrete type and cannot be type
	// asserted to *net.TCPConn, etc.
	//
	// The rawConn represents the underlying connections (with
	// some bytes removed) and should only be used for type
	// assertions and setting deadlines, not reading.
	HandleConn(c net.Conn, rawConn net.Conn)
}

// To is shorthand way of writing &DialProxy{Addr: addr}.
func To(addr string) *DialProxy {
	return &DialProxy{Addr: addr}
}

// DialProxy implements Target by dialing a new connection to Addr
// and then proxying data back and forth.
//
// The To func is a shorthand way of creating a DialProxy.
type DialProxy struct {
	// Addr is the TCP address to proxy to.
	Addr string

	// KeepAlivePeriod sets the period between TCP keep alives.
	// If zero, a default is used. To disable, use a negative number.
	// The keep-alive is used for both the client connection and
	KeepAlivePeriod time.Duration

	// DialTimeout optionally specifies a dial timeout.
	// If zero, a default is used.
	// If negative, the timeout is disabled.
	DialTimeout time.Duration

	// DialContext optionally specifies an alternate dial function
	// for TCP targets. If nil, the standard
	// net.Dialer.DialContext method is used.
	DialContext func(ctx context.Context, network, address string) (net.Conn, error)

	// OnDialError optionally specifies an alternate way to handle errors dialing Addr.
	// If nil, the error is logged and src is closed.
	// If non-nil, src is not closed automatically.
	OnDialError func(src net.Conn, dstDialErr error)
}

func (dp *DialProxy) HandleConn(src net.Conn, rawSrc net.Conn) {
	ctx := context.Background()
	var cancel context.CancelFunc
	if dp.DialTimeout >= 0 {
		ctx, cancel = context.WithTimeout(ctx, dp.dialTimeout())
	}
	dst, err := dp.dialContext()(ctx, "tcp", dp.Addr)
	if cancel != nil {
		cancel()
	}
	if err != nil {
		dp.onDialError()(src, err)
		return
	}
	defer src.Close()
	defer dst.Close()
	if ka := dp.keepAlivePeriod(); ka > 0 {
		if c, ok := rawSrc.(*net.TCPConn); ok {
			c.SetKeepAlive(true)
			c.SetKeepAlivePeriod(ka)
		}
		if c, ok := dst.(*net.TCPConn); ok {
			c.SetKeepAlive(true)
			c.SetKeepAlivePeriod(ka)
		}
	}
	errc := make(chan error, 1)
	go proxyCopy(errc, src, dst)
	go proxyCopy(errc, dst, src)
	<-errc
}

// proxyCopy is the function that copies bytes around.
// It's a named function instead of a func literal so users get
// named goroutines in debug goroutine stack dumps.
func proxyCopy(errc chan<- error, dst io.Writer, src io.Reader) {
	// TODO: make caller switch from src to rawSrc after N bytes (e.g. 4KB)
	// if the io.Copy optimization to switch to Linux splice happens.
	// TODO: if the runtime provides a way to wait for
	// readability, use that to avoid stranding big blocks of
	// memory blocked in idle reads.
	_, err := io.Copy(dst, src)
	errc <- err
}

func (dp *DialProxy) keepAlivePeriod() time.Duration {
	if dp.KeepAlivePeriod != 0 {
		return dp.KeepAlivePeriod
	}
	return time.Minute
}

func (dp *DialProxy) dialTimeout() time.Duration {
	if dp.DialTimeout > 0 {
		return dp.DialTimeout
	}
	return 10 * time.Second
}

var defaultDialer = new(net.Dialer)

func (dp *DialProxy) dialContext() func(ctx context.Context, network, address string) (net.Conn, error) {
	if dp.DialContext != nil {
		return dp.DialContext
	}
	return defaultDialer.DialContext
}

func (dp *DialProxy) onDialError() func(src net.Conn, dstDialErr error) {
	if dp.OnDialError != nil {
		return dp.OnDialError
	}
	return func(src net.Conn, dstDialErr error) {
		log.Printf("tcpproxy: for incoming conn %v, error dialing %q: %v", src.RemoteAddr().String(), dp.Addr, dstDialErr)
		src.Close()
	}
}