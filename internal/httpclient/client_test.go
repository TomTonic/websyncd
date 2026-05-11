package httpclient

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type recordingDoer struct {
	calls int
	err   error
	resp  *http.Response
}

func (r *recordingDoer) Do(_ *http.Request) (*http.Response, error) {
	r.calls++
	return r.resp, r.err
}

// TestFallbackDoerFallsBackWhenPrimaryFails verifies that enabling HTTP/3 does
// not break sync when the QUIC transport is unavailable.
//
// This test covers the fallbackDoer type in the httpclient package, which
// transparently retries failed HTTP/3 requests over a standard HTTP transport.
//
// It configures a primary client that always errors and a fallback that
// succeeds, then asserts the response comes from the fallback and that both
// clients were invoked exactly once.
func TestFallbackDoerFallsBackWhenPrimaryFails(t *testing.T) {
	// User perspective: enabling HTTP/3 must not break sync when QUIC fails.
	// System perspective: failed primary request should transparently retry on fallback client.
	// Code perspective: fallbackDoer invokes fallback Do after primary error.
	primary := &recordingDoer{err: errors.New("h3 not available")}
	fallback := &recordingDoer{resp: &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("ok"))}}
	d := &fallbackDoer{primary: primary, fallback: fallback}

	req, _ := http.NewRequest(http.MethodGet, "https://example.invalid/resource", nil)
	resp, err := d.Do(req)
	if err != nil {
		t.Fatalf("Do() error = %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if primary.calls != 1 || fallback.calls != 1 {
		t.Fatalf("calls primary=%d fallback=%d", primary.calls, fallback.calls)
	}
}

// TestNewReturnsStdClientWhenHTTP3Disabled verifies that the default runtime
// works without HTTP/3 and that no extra resources need to be cleaned up.
//
// This test covers the New constructor in the httpclient package.
//
// It calls New with enableHTTP3=false and asserts that the returned client is a
// plain *http.Client and that the CloseFunc completes without error.
func TestNewReturnsStdClientWhenHTTP3Disabled(t *testing.T) {
	// User perspective: default runtime should work without HTTP/3.
	// System perspective: disabled HTTP/3 uses standard net/http client only.
	// Code perspective: New returns a non-fallback client and a closable no-op.
	client, closeFn := New(2*time.Second, false)
	if _, ok := client.(*http.Client); !ok {
		t.Fatalf("expected standard http.Client when HTTP/3 disabled")
	}
	if err := closeFn(); err != nil {
		t.Fatalf("closeFn() error = %v", err)
	}
}
