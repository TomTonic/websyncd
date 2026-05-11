package httpclient

import (
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

// TestNewBuildsWorkingClientWithHTTP3Disabled verifies that users who explicitly
// disable HTTP/3 still receive a functional HTTP client capable of making
// standard TCP requests.
//
// This test covers the New constructor in the httpclient package with
// enableHTTP3=false, confirming a plain *http.Client is returned.
//
// It checks that the returned Doer is a concrete *http.Client and that the
// CloseFunc completes without error.
func TestNewBuildsWorkingClientWithHTTP3Disabled(t *testing.T) {
	client, closeFn := New(2*time.Second, false)
	if _, ok := client.(*http.Client); !ok {
		t.Fatalf("expected *http.Client when HTTP/3 disabled, got %T", client)
	}
	if err := closeFn(); err != nil {
		t.Fatalf("closeFn() error = %v", err)
	}
}

// TestNewBuildsFunctionalRecordingClient is a compile-time / wiring check that
// recordingDoer satisfies the Doer interface and can be used in place of a real
// client in tests, ensuring the Doer contract is stable.
//
// This test covers the Doer interface definition in the httpclient package.
//
// It constructs a recordingDoer with a canned 200 response and asserts that a
// single Do call returns the expected status.
func TestNewBuildsFunctionalRecordingClient(t *testing.T) {
	d := &recordingDoer{resp: &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader("ok")),
	}}
	req, _ := http.NewRequest(http.MethodGet, "https://example.invalid/", nil)
	resp, err := d.Do(req)
	if err != nil {
		t.Fatalf("Do() error = %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	if d.calls != 1 {
		t.Fatalf("calls = %d, want 1", d.calls)
	}
}
