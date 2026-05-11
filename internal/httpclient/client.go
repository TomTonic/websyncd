// Package httpclient provides an HTTP client factory with optional HTTP/3 support
// and automatic fallback to HTTP/1.1 or HTTP/2.
package httpclient

import (
	"errors"
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
// timeout is applied to every individual request. enableHTTP3, when true,
// wraps the client with an HTTP/3 (QUIC) transport that automatically falls
// back to HTTP/1.1 or HTTP/2 if QUIC fails.
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
	fallback := &http.Client{Timeout: timeout}
	if !enableHTTP3 {
		return fallback, func() error { return nil }
	}

	h3Transport := &http3.Transport{}
	h3Client := &http.Client{
		Timeout:   timeout,
		Transport: h3Transport,
	}

	return &fallbackDoer{primary: h3Client, fallback: fallback}, h3Transport.Close
}

type fallbackDoer struct {
	primary  Doer
	fallback Doer
}

func (f *fallbackDoer) Do(req *http.Request) (*http.Response, error) {
	resp, err := f.primary.Do(req)
	if err == nil {
		return resp, nil
	}

	if req.Body != nil {
		return nil, err
	}

	cloned := req.Clone(req.Context())
	fallbackResp, fallbackErr := f.fallback.Do(cloned)
	if fallbackErr != nil {
		return nil, errors.Join(err, fallbackErr)
	}
	return fallbackResp, nil
}
