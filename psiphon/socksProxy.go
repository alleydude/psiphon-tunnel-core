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
	"fmt"
	socks "github.com/Psiphon-Inc/goptlib"
	"net"
	"sync"
)

// SocksProxy is a SOCKS server that accepts local host connections
// and, for each connection, establishes a port forward through
// the tunnel SSH client and relays traffic through the port
// forward.
type SocksProxy struct {
	tunneler       Tunneler
	listener       *socks.SocksListener
	serveWaitGroup *sync.WaitGroup
	openConns      *Conns
}

// NewSocksProxy initializes a new SOCKS server. It begins listening for
// connections, starts a goroutine that runs an accept loop, and returns
// leaving the accept loop running.
func NewSocksProxy(config *Config, tunneler Tunneler) (proxy *SocksProxy, err error) {
	listener, err := socks.ListenSocks(
		"tcp", fmt.Sprintf("127.0.0.1:%d", config.LocalSocksProxyPort))
	if err != nil {
		return nil, ContextError(err)
	}
	proxy = &SocksProxy{
		tunneler:       tunneler,
		listener:       listener,
		serveWaitGroup: new(sync.WaitGroup),
		openConns:      new(Conns),
	}
	proxy.serveWaitGroup.Add(1)
	go proxy.serve()
	Notice(NOTICE_SOCKS_PROXY, "local SOCKS proxy running at address %s", proxy.listener.Addr().String())
	return proxy, nil
}

// Close terminates the listener and waits for the accept loop
// goroutine to complete.
func (proxy *SocksProxy) Close() {
	proxy.listener.Close()
	proxy.serveWaitGroup.Wait()
	proxy.openConns.CloseAll()
}

func (proxy *SocksProxy) socksConnectionHandler(localConn *socks.SocksConn) (err error) {
	defer localConn.Close()
	defer proxy.openConns.Remove(localConn)
	proxy.openConns.Add(localConn)
	remoteConn, err := proxy.tunneler.Dial(localConn.Req.Target)
	if err != nil {
		return ContextError(err)
	}
	defer remoteConn.Close()
	err = localConn.Grant(&net.TCPAddr{IP: net.ParseIP("0.0.0.0"), Port: 0})
	if err != nil {
		return ContextError(err)
	}
	Relay(localConn, remoteConn)
	return nil
}

func (proxy *SocksProxy) serve() {
	defer proxy.listener.Close()
	defer proxy.serveWaitGroup.Done()
	for {
		// Note: will be interrupted by listener.Close() call made by proxy.Close()
		socksConnection, err := proxy.listener.AcceptSocks()
		if err != nil {
			Notice(NOTICE_ALERT, "SOCKS proxy accept error: %s", err)
			if e, ok := err.(net.Error); ok && !e.Temporary() {
				proxy.tunneler.SignalFailure()
				// Fatal error, stop the proxy
				break
			}
			// Temporary error, keep running
			continue
		}
		go func() {
			err := proxy.socksConnectionHandler(socksConnection)
			if err != nil {
				Notice(NOTICE_ALERT, "%s", ContextError(err))
			}
		}()
	}
	Notice(NOTICE_SOCKS_PROXY, "SOCKS proxy stopped")
}
