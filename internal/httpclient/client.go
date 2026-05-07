package httpclient

import (
	"errors"
	"net/http"
	"time"

	"github.com/quic-go/quic-go/http3"
)

type Doer interface {
	Do(req *http.Request) (*http.Response, error)
}

type CloseFunc func() error

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
