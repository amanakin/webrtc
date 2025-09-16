// SPDX-FileCopyrightText: 2023 The Pion community <https://pion.ly>
// SPDX-License-Identifier: MIT

package httpproxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"

	"golang.org/x/net/proxy"
)

func init() {
	proxy.RegisterDialerType("http", New)
	proxy.RegisterDialerType("https", New)
}

const (
	// proxyDialingTimeout is the timeout for establishing a TCP connection with the proxy.
	proxyDialingTimeout = 20 * time.Second
	// proxyConnectionTimeout is the timeout for the proxy to handle the CONNECT verb
	// (e.g. proxy establishes a TCP connection with destination).
	proxyConnectionTimeout = 20 * time.Second
	// maxProxyResponseSize is the maximum size of the response body that
	// will be read from the proxy in case of an error.
	maxProxyResponseSize = 10 * 1024 // 10KB
)

var _ proxy.Dialer = &httpDialer{}

type httpDialer struct {
	forward proxy.Dialer

	proxyAddr string
	userinfo  *url.Userinfo
	https     bool
}

// New creates a new proxy dialer with the given proxy address.
func New(u *url.URL, forward proxy.Dialer) (proxy.Dialer, error) {
	var https bool
	switch u.Scheme {
	case "http":
		https = false
	case "https":
		https = true
	default:
		return nil, fmt.Errorf("unsupported scheme in proxy URL: %v", u.Scheme)
	}

	return &httpDialer{
		forward:   forward,
		proxyAddr: u.Host,
		userinfo:  u.User,
		https:     https,
	}, nil
}

func (d *httpDialer) Dial(network, addr string) (net.Conn, error) {
	return d.DialContext(context.Background(), network, addr)
}

func (d *httpDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	if network != "tcp" && network != "tcp4" && network != "tcp6" {
		return nil, fmt.Errorf("unsupported network type: %v", network)
	}

	// Dial the proxy.
	var (
		conn net.Conn
		err  error
	)
	if contextDialer, ok := d.forward.(proxy.ContextDialer); ok {
		ctx, cancel := context.WithTimeout(ctx, proxyDialingTimeout)
		defer cancel()
		conn, err = contextDialer.DialContext(ctx, network, d.proxyAddr)
	} else {
		conn, err = d.forward.Dial(network, d.proxyAddr)
	}
	if err != nil {
		return nil, fmt.Errorf("connect to proxy %q: %w", d.proxyAddr, err)
	}

	// Set a deadline for R/W operations on the proxy connection.
	conn.SetDeadline(time.Now().Add(proxyConnectionTimeout))

	if d.https {
		host, _, err := net.SplitHostPort(d.proxyAddr)
		if err != nil {
			host = d.proxyAddr
		}
		conn = tls.Client(conn, &tls.Config{ServerName: host})
	}

	// Create a CONNECT request to the proxy with target address.
	req := &http.Request{
		Method: http.MethodConnect,
		URL:    &url.URL{Host: addr},
		Header: http.Header{
			"Proxy-Connection": []string{"Keep-Alive"},
		},
	}
	if d.userinfo != nil {
		req.Header.Set("Proxy-Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(d.userinfo.String())))
	}

	err = req.Write(conn)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("write request: %w", err)
	}

	resp, err := http.ReadResponse(bufio.NewReader(conn), req)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("read response: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, err := io.ReadAll(io.LimitReader(resp.Body, maxProxyResponseSize))
		if err != nil {
			conn.Close()
			return nil, fmt.Errorf("connect status: %d %q, read response: %w", resp.StatusCode, resp.Status, err)
		}
		conn.Close()
		return nil, fmt.Errorf("connect status: %d %q, response body: %q", resp.StatusCode, resp.Status, string(body))
	}

	// Clear the deadline for the operations on the proxy connection.
	conn.SetDeadline(time.Time{})
	return conn, nil
}
