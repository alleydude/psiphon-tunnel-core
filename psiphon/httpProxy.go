/*
 * Copyright (c) 2014, Psiphon Inc.
 * All rights reserved.
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 *
 */

package psiphon

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
)

// HttpProxy is a HTTP server that relays HTTP requests through
// the tunnel SSH client.
type HttpProxy struct {
	controller     *Controller
	listener       net.Listener
	serveWaitGroup *sync.WaitGroup
	httpRelay      *http.Transport
	openConns      *Conns
}

// NewHttpProxy initializes and runs a new HTTP proxy server.
func NewHttpProxy(controller *Controller) (proxy *HttpProxy, err error) {
	listener, err := net.Listen(
		"tcp", fmt.Sprintf("127.0.0.1:%d", controller.config.LocalHttpProxyPort))
	if err != nil {
		return nil, ContextError(err)
	}
	tunneledDialer := func(_, addr string) (conn net.Conn, err error) {
		// TODO: connect timeout?
		return controller.dialWithTunnel(addr)
	}
	// TODO: also use http.Client, with its Timeout field?
	transport := &http.Transport{
		Dial:                  tunneledDialer,
		MaxIdleConnsPerHost:   HTTP_PROXY_MAX_IDLE_CONNECTIONS_PER_HOST,
		ResponseHeaderTimeout: HTTP_PROXY_ORIGIN_SERVER_TIMEOUT,
	}
	proxy = &HttpProxy{
		controller:     controller,
		listener:       listener,
		serveWaitGroup: new(sync.WaitGroup),
		httpRelay:      transport,
		openConns:      new(Conns),
	}
	proxy.serveWaitGroup.Add(1)
	go proxy.serve()
	Notice(NOTICE_HTTP_PROXY, "local HTTP proxy running at address %s", proxy.listener.Addr().String())
	return proxy, nil
}

// Close terminates the HTTP server.
func (proxy *HttpProxy) Close() {
	proxy.listener.Close()
	proxy.serveWaitGroup.Wait()
	// Close local->proxy persistent connections
	proxy.openConns.CloseAll()
	// Close idle proxy->origin persistent connections
	// TODO: also close active connections
	proxy.httpRelay.CloseIdleConnections()
}

// ServeHTTP receives HTTP requests and proxies them. CONNECT requests
// are hijacked and all data is relayed. Other HTTP requests are proxied
// with explicit round trips. In both cases, the tunnel is used for proxied
// traffic.
//
// Implementation is based on:
//
// https://github.com/justmao945/mallory
// Copyright (c) 2014 JianjunMao
// The MIT License (MIT)
//
// https://golang.org/src/pkg/net/http/httputil/reverseproxy.go
// Copyright 2011 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.
//
func (proxy *HttpProxy) ServeHTTP(responseWriter http.ResponseWriter, request *http.Request) {
	if request.Method == "CONNECT" {
		hijacker, _ := responseWriter.(http.Hijacker)
		conn, _, err := hijacker.Hijack()
		if err != nil {
			Notice(NOTICE_ALERT, "%s", ContextError(err))
			http.Error(responseWriter, "", http.StatusInternalServerError)
			return
		}
		go func() {
			err := proxy.httpConnectHandler(conn, request.URL.Host)
			if err != nil {
				Notice(NOTICE_ALERT, "%s", ContextError(err))
			}
		}()
		return
	}
	if !request.URL.IsAbs() {
		Notice(NOTICE_ALERT, "%s", ContextError(errors.New("no domain in request URL")))
		http.Error(responseWriter, "", http.StatusInternalServerError)
		return
	}

	// Transform request struct before using as input to relayed request
	request.Close = false
	request.RequestURI = ""
	for _, key := range hopHeaders {
		request.Header.Del(key)
	}

	// Relay the HTTP request and get the response
	response, err := proxy.httpRelay.RoundTrip(request)
	if err != nil {
		Notice(NOTICE_ALERT, "%s", ContextError(err))
		forceClose(responseWriter)
		return
	}
	defer response.Body.Close()

	// Relay the remote response headers
	for _, key := range hopHeaders {
		response.Header.Del(key)
	}
	for key, _ := range responseWriter.Header() {
		responseWriter.Header().Del(key)
	}
	for key, values := range response.Header {
		for _, value := range values {
			responseWriter.Header().Add(key, value)
		}
	}

	// Relay the response code and body
	responseWriter.WriteHeader(response.StatusCode)
	_, err = io.Copy(responseWriter, response.Body)
	if err != nil {
		Notice(NOTICE_ALERT, "%s", ContextError(err))
		forceClose(responseWriter)
		return
	}
}

// forceClose hijacks and closes persistent connections. This is used
// to ensure local persistent connections into the HTTP proxy are closed
// when ServeHTTP encounters an error.
func forceClose(responseWriter http.ResponseWriter) {
	hijacker, _ := responseWriter.(http.Hijacker)
	conn, _, err := hijacker.Hijack()
	if err == nil {
		conn.Close()
	}
}

// From // https://golang.org/src/pkg/net/http/httputil/reverseproxy.go:
// Hop-by-hop headers. These are removed when sent to the backend.
// http://www.w3.org/Protocols/rfc2616/rfc2616-sec13.html
var hopHeaders = []string{
	"Connection",
	"Keep-Alive",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"Proxy-Connection", // see: http://homepage.ntlworld.com/jonathan.deboynepollard/FGA/web-proxy-connection-header.html
	"Te",               // canonicalized version of "TE"
	"Trailers",
	"Transfer-Encoding",
	"Upgrade",
}

func (proxy *HttpProxy) httpConnectHandler(localConn net.Conn, target string) (err error) {
	defer localConn.Close()
	defer proxy.openConns.Remove(localConn)
	proxy.openConns.Add(localConn)
	remoteConn, err := proxy.controller.dialWithTunnel(target)
	if err != nil {
		return ContextError(err)
	}
	defer remoteConn.Close()
	_, err = localConn.Write([]byte("HTTP/1.1 200 OK\r\n\r\n"))
	if err != nil {
		return ContextError(err)
	}
	Relay(localConn, remoteConn)
	return nil
}

// httpConnStateCallback is called by http.Server when the state of a local->proxy
// connection changes. Open connections are tracked so that all local->proxy persistent
// connections can be closed by HttpProxy.Close()
// TODO: if the HttpProxy is decoupled from a single Tunnel instance and
// instead uses the "current" Tunnel, it may not be necessary to close
// local persistent connections when the tunnel reconnects.
func (proxy *HttpProxy) httpConnStateCallback(conn net.Conn, connState http.ConnState) {
	switch connState {
	case http.StateNew:
		proxy.openConns.Add(conn)
	case http.StateActive, http.StateIdle:
		// No action
	case http.StateHijacked, http.StateClosed:
		proxy.openConns.Remove(conn)
	}
}

func (proxy *HttpProxy) serve() {
	defer proxy.listener.Close()
	defer proxy.serveWaitGroup.Done()
	httpServer := &http.Server{
		Handler:   proxy,
		ConnState: proxy.httpConnStateCallback,
	}
	// Note: will be interrupted by listener.Close() call made by proxy.Close()
	err := httpServer.Serve(proxy.listener)
	if err != nil {
		proxy.controller.SignalFailure()
		Notice(NOTICE_ALERT, "%s", ContextError(err))
	}
	Notice(NOTICE_HTTP_PROXY, "HTTP proxy stopped")
}