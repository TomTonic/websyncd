// Package httpclient provides an HTTP client factory with optional HTTP/3
// support via Alt-Svc-based auto-upgrade.
package httpclient

import (
	"net/http"
	"time"

	"github.com/quic-go/quic-go/http3"
)

// Doer is the minimal HTTP client interface required by websyncd.
// Any *http.Client satisfies this interface. Accepting Doer instead of a
// concrete type allows tests to inject fake transports without a live network.
type Doer interface {
	Do(req *http.Request) (*http.Response, error)
}

// CloseFunc releases resources held by the HTTP client returned by New.
// Callers must invoke CloseFunc (typically via defer) when the client is no
// longer needed. When HTTP/3 is disabled the returned CloseFunc is a no-op.
type CloseFunc func() error

// New constructs an HTTP client suitable for the given configuration.
//
// timeout is applied to every individual request. enableHTTP3, when true
// (the default), wraps the standard transport with an Alt-Svc-aware
// RoundTripper: the first request to an origin uses TCP; if the response
// carries an Alt-Svc header advertising h3 the origin is promoted to HTTP/3
// for subsequent requests. A per-origin cooldown prevents repeated QUIC
// attempts when UDP is blocked.
//
// When enableHTTP3 is false a plain *http.Client is returned without any
// HTTP/3 capability.
//
// Returns a Doer for making requests and a CloseFunc that must be called on
// shutdown to release transport resources. The CloseFunc is safe to call
// multiple times.
//
// Typical usage:
//
//	client, closeClient := httpclient.New(cfg.HTTPTimeout, cfg.EnableHTTP3)
//	defer closeClient()
func New(timeout time.Duration, enableHTTP3 bool) (Doer, CloseFunc) {
	tcpTransport := http.DefaultTransport
	tcpClient := &http.Client{Timeout: timeout}

	if !enableHTTP3 {
		return tcpClient, func() error { return nil }
	}

	h3Transport := &http3.Transport{}
	transport := &altSvcTransport{
		tcp:   tcpTransport,
		h3:    h3Transport,
		cache: newAltSvcCache(),
	}
	client := &http.Client{
		Timeout:   timeout,
		Transport: transport,
	}
	return client, h3Transport.Close
}
