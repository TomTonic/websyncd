package syncer

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type stubDoer struct {
	fn func(*http.Request) (*http.Response, error)
}

func (s *stubDoer) Do(req *http.Request) (*http.Response, error) {
	return s.fn(req)
}

func makeResp(code int, headers map[string]string, body string) *http.Response {
	r := &http.Response{
		StatusCode: code,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader(body)),
		Status:     http.StatusText(code),
	}
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	return r
}

// TestSync_ClientRequired verifies Sync returns an error when no HTTP client is configured.
//
// This test covers the Sync precondition that requires a non-nil HTTP client.
// It constructs a Syncer with a nil Client and asserts Sync returns an error.
func TestSync_ClientRequired(t *testing.T) {
	s := &Syncer{Client: nil, Resource: "https://example.invalid/res", OutputPath: filepath.Join(t.TempDir(), "out")}
	if err := s.Sync(context.Background()); err == nil {
		t.Fatalf("Sync() succeeded without client, want error")
	}
}

// TestHeadBehaviors exercises head response handling across common server responses.
//
// This test covers the head() decision logic for 405/304/2xx/4xx responses and cache validator matching.
// It asserts that the returned needGET boolean matches the expected behavior for each case.
func TestHeadBehaviors(t *testing.T) {
	cases := []struct {
		name     string
		code     int
		hdrs     map[string]string
		setETag  string
		setLM    string
		wantNeed bool
	}{
		{"methodNotAllowed", http.StatusMethodNotAllowed, nil, "", "", true},
		{"notModified", http.StatusNotModified, nil, "", "", false},
		{"badStatus", http.StatusBadRequest, nil, "", "", true},
		{"etagMatch", http.StatusOK, map[string]string{"ETag": "a1"}, "a1", "", false},
		{"lastModifiedMatch", http.StatusOK, map[string]string{"Last-Modified": "tm"}, "", "tm", false},
		{"noMatch", http.StatusOK, map[string]string{"ETag": "x", "Last-Modified": "y"}, "a", "b", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &Syncer{
				Client: &stubDoer{fn: func(req *http.Request) (*http.Response, error) {
					return makeResp(tc.code, tc.hdrs, ""), nil
				}},
				Resource: filepath.Join(t.TempDir(), "unused"),
			}
			s.etag = tc.setETag
			s.lastModified = tc.setLM
			resp, need, err := s.head(context.Background())
			if err != nil {
				t.Fatalf("head() error = %v", err)
			}
			if need != tc.wantNeed {
				t.Fatalf("head() need = %v, want %v (resp=%v)", need, tc.wantNeed, resp)
			}
		})
	}

	t.Run("do error", func(t *testing.T) {
		s := &Syncer{Client: &stubDoer{fn: func(req *http.Request) (*http.Response, error) {
			return nil, errors.New("network")
		}}, Resource: "https://x"}
		if _, _, err := s.head(context.Background()); err == nil {
			t.Fatalf("head() succeeded when Do returned error")
		}
	})
}

// TestWriteAtomically verifies atomic writes create the target file and handle directory permission errors.
//
// This test covers writeAtomically success path (create in nested directory and write contents)
// and failure when the target directory is not writable.
func TestWriteAtomically(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		tmp := t.TempDir()
		out := filepath.Join(tmp, "sub", "out.txt")
		s := &Syncer{OutputPath: out}
		if err := s.writeAtomically(io.NopCloser(strings.NewReader("hello"))); err != nil {
			t.Fatalf("writeAtomically() error = %v", err)
		}
		b, err := os.ReadFile(out)
		if err != nil {
			t.Fatalf("ReadFile() error = %v", err)
		}
		if string(b) != "hello" {
			t.Fatalf("file content = %q, want %q", string(b), "hello")
		}
	})

	t.Run("create fail", func(t *testing.T) {
		tmp := t.TempDir()
		dir := filepath.Join(tmp, "nowrite")
		if err := os.MkdirAll(dir, 0o500); err != nil {
			t.Fatalf("MkdirAll() error = %v", err)
		}
		out := filepath.Join(dir, "out.txt")
		s := &Syncer{OutputPath: out}
		defer os.Chmod(dir, 0o700)
		if err := s.writeAtomically(io.NopCloser(strings.NewReader("x"))); err == nil {
			t.Fatalf("writeAtomically() succeeded unexpectedly")
		}
	})
}

// TestSyncFlows groups subtests covering Sync behavior for NotModified, successful GET, and GET failure.
//
// This test covers end-to-end Sync flows including how head errors are handled, successful body writes,
// and proper error propagation on non-2xx GET responses.
func TestSyncFlows(t *testing.T) {
	t.Run("GET NotModified", func(t *testing.T) {
		tmp := t.TempDir()
		out := filepath.Join(tmp, "out.txt")
		client := &stubDoer{fn: func(req *http.Request) (*http.Response, error) {
			if req.Method == http.MethodHead {
				return nil, errors.New("head fail")
			}
			if req.Method == http.MethodGet {
				return makeResp(http.StatusNotModified, nil, ""), nil
			}
			return nil, errors.New("unexpected")
		}}
		s := &Syncer{Client: client, Resource: "https://example.invalid/res", OutputPath: out}
		if err := s.Sync(context.Background()); err != nil {
			t.Fatalf("Sync() error = %v", err)
		}
		if _, err := os.Stat(out); err == nil {
			t.Fatalf("file was created unexpectedly")
		} else if !os.IsNotExist(err) {
			t.Fatalf("unexpected stat error: %v", err)
		}
	})

	t.Run("GET Success writes file and updates validators", func(t *testing.T) {
		tmp := t.TempDir()
		out := filepath.Join(tmp, "out.txt")
		client := &stubDoer{fn: func(req *http.Request) (*http.Response, error) {
			if req.Method == http.MethodHead {
				return makeResp(http.StatusMethodNotAllowed, nil, ""), nil
			}
			if req.Method == http.MethodGet {
				return makeResp(http.StatusOK, map[string]string{"ETag": "v1", "Last-Modified": "lm"}, "bodycontent"), nil
			}
			return nil, errors.New("unexpected")
		}}
		s := &Syncer{Client: client, Resource: "https://example.invalid/res", OutputPath: out}
		if err := s.Sync(context.Background()); err != nil {
			t.Fatalf("Sync() error = %v", err)
		}
		b, err := os.ReadFile(out)
		if err != nil {
			t.Fatalf("ReadFile() error = %v", err)
		}
		if string(b) != "bodycontent" {
			t.Fatalf("file content = %q, want %q", string(b), "bodycontent")
		}
		if s.etag != "v1" {
			t.Fatalf("etag = %q, want %q", s.etag, "v1")
		}
		if s.lastModified != "lm" {
			t.Fatalf("lastModified = %q, want %q", s.lastModified, "lm")
		}
	})

	t.Run("GET Failure returns error", func(t *testing.T) {
		tmp := t.TempDir()
		out := filepath.Join(tmp, "out.txt")
		client := &stubDoer{fn: func(req *http.Request) (*http.Response, error) {
			if req.Method == http.MethodHead {
				return makeResp(http.StatusMethodNotAllowed, nil, ""), nil
			}
			if req.Method == http.MethodGet {
				return makeResp(http.StatusInternalServerError, nil, ""), nil
			}
			return nil, errors.New("unexpected")
		}}
		s := &Syncer{Client: client, Resource: "https://example.invalid/res", OutputPath: out}
		if err := s.Sync(context.Background()); err == nil {
			t.Fatalf("Sync() succeeded unexpectedly on GET 500")
		}
	})
}

type response struct {
	resp *http.Response
	err  error
}

type fakeClient struct {
	mu       sync.Mutex
	calls    []string
	queue    []response
	headers  []http.Header
	contexts []context.Context
}

func (f *fakeClient) Do(req *http.Request) (*http.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, req.Method)
	f.headers = append(f.headers, req.Header.Clone())
	f.contexts = append(f.contexts, req.Context())
	if len(f.queue) == 0 {
		return nil, errors.New("unexpected request")
	}
	item := f.queue[0]
	f.queue = f.queue[1:]
	return item.resp, item.err
}

// TestSyncSkipsGetWhenHeadReturns304 verifies that an unchanged remote resource
// does not trigger a re-download, avoiding unnecessary bandwidth and file I/O.
//
// This test covers the Sync method in the syncer package.
//
// It queues a single HEAD 304 response and asserts that Sync returns nil after
// only one request, with no GET issued.
func TestSyncSkipsGetWhenHeadReturns304(t *testing.T) {
	// User perspective: no remote change should avoid re-downloading content.
	// System perspective: HEAD 304 means local file is already current.
	// Code perspective: Sync must stop after HEAD and never call GET.
	dir := t.TempDir()
	out := filepath.Join(dir, "state.txt")
	fc := &fakeClient{
		queue: []response{{
			resp: &http.Response{
				StatusCode: http.StatusNotModified,
				Body:       io.NopCloser(strings.NewReader("")),
				Header:     make(http.Header),
			},
		}},
	}

	s := &Syncer{Client: fc, Resource: "https://example.invalid/resource", OutputPath: out}
	if err := s.Sync(context.Background()); err != nil {
		t.Fatalf("Sync() error = %v", err)
	}
	if len(fc.calls) != 1 || fc.calls[0] != http.MethodHead {
		t.Fatalf("expected only HEAD request, got %v", fc.calls)
	}
}

// TestSyncDownloadsWhenHeadChanged verifies that a changed remote resource is
// fetched and written to disk so users always have the latest content.
//
// This test covers the Sync method in the syncer package.
//
// It queues a HEAD 200 (new ETag) followed by GET 200 with body "new-data" and
// asserts that both requests are made and the file contains the expected bytes.
func TestSyncDownloadsWhenHeadChanged(t *testing.T) {
	// User perspective: changed remote content should be downloaded.
	// System perspective: HEAD indicates change, then GET fetches bytes.
	// Code perspective: Sync should issue HEAD then GET and persist body atomically.
	dir := t.TempDir()
	out := filepath.Join(dir, "state.txt")
	head := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Etag": []string{"v2"}}, Body: io.NopCloser(strings.NewReader(""))}
	get := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Etag": []string{"v2"}}, Body: io.NopCloser(strings.NewReader("new-data"))}
	fc := &fakeClient{queue: []response{{resp: head}, {resp: get}}}

	s := &Syncer{Client: fc, Resource: "https://example.invalid/resource", OutputPath: out}
	if err := s.Sync(context.Background()); err != nil {
		t.Fatalf("Sync() error = %v", err)
	}
	if got, want := strings.Join(fc.calls, ","), "HEAD,GET"; got != want {
		t.Fatalf("calls = %s, want %s", got, want)
	}
	content, err := os.ReadFile(out) //nolint:gosec // G304: test uses a temp-dir path from t.TempDir
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if string(content) != "new-data" {
		t.Fatalf("content = %q, want %q", string(content), "new-data")
	}
}

// TestSyncReplacesTargetAtomically verifies that an in-progress download never
// leaves a partially written file visible to readers.
//
// This test covers the writeAtomically helper used by Sync in the syncer package.
//
// It pre-creates the target file, triggers a sync, and then asserts the file
// contains the full new payload and no temporary files remain in the directory.
func TestSyncReplacesTargetAtomically(t *testing.T) {
	// User perspective: updated file should never be partially written.
	// System perspective: download writes temp file and renames over target.
	// Code perspective: existing target must become complete new payload.
	dir := t.TempDir()
	out := filepath.Join(dir, "state.txt")
	if err := os.WriteFile(out, []byte("old"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	head := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Etag": []string{"v2"}}, Body: io.NopCloser(strings.NewReader(""))}
	get := &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("new"))}
	fc := &fakeClient{queue: []response{{resp: head}, {resp: get}}}

	s := &Syncer{Client: fc, Resource: "https://example.invalid/resource", OutputPath: out}
	if err := s.Sync(context.Background()); err != nil {
		t.Fatalf("Sync() error = %v", err)
	}

	content, err := os.ReadFile(out) //nolint:gosec // G304: test uses a temp-dir path from t.TempDir
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if string(content) != "new" {
		t.Fatalf("content = %q, want %q", string(content), "new")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir() error = %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".websyncd-") {
			t.Fatalf("unexpected temp file left behind: %s", e.Name())
		}
	}
}

type cancelReadCloser struct {
	ctx context.Context
}

func (r *cancelReadCloser) Read(_ []byte) (int, error) {
	<-r.ctx.Done()
	return 0, r.ctx.Err()
}

func (r *cancelReadCloser) Close() error { return nil }

// TestSyncCancellationStopsDownloadAndCleansTempFile verifies that shutting down
// the service while a download is in progress does not leave stale temp files.
//
// This test covers context-cancellation handling in the Sync method of the
// syncer package.
//
// It uses a fake body whose Read blocks until the context is cancelled, then
// asserts that Sync returns context.Canceled and that no temporary file remains
// in the output directory.
func TestSyncCancellationStopsDownloadAndCleansTempFile(t *testing.T) {
	// User perspective: service shutdown should abort in-progress download safely.
	// System perspective: canceled context must stop stream and skip replacing target.
	// Code perspective: io.Copy should return context cancellation and cleanup temp file.
	dir := t.TempDir()
	out := filepath.Join(dir, "state.txt")
	ctx, cancel := context.WithCancel(context.Background())

	head := &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(""))}
	get := &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: &cancelReadCloser{ctx: ctx}}
	fc := &fakeClient{queue: []response{{resp: head}, {resp: get}}}

	s := &Syncer{Client: fc, Resource: "https://example.invalid/resource", OutputPath: out}
	done := make(chan error, 1)
	go func() {
		done <- s.Sync(ctx)
	}()

	time.Sleep(20 * time.Millisecond)
	cancel()

	err := <-done
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Sync() error = %v, want context canceled", err)
	}
	entries, readErr := os.ReadDir(dir)
	if readErr != nil {
		t.Fatalf("ReadDir() error = %v", readErr)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".websyncd-") {
			t.Fatalf("unexpected temp file left behind: %s", e.Name())
		}
	}
}

// TestHeadUsesConditionalHeaders verifies that repeated syncs send HTTP cache
// validators so that an unchanged resource requires only one round-trip.
//
// This test covers the conditional-request logic in the Sync method of the
// syncer package.
//
// It performs two syncs: the first fetches real content, the second sends
// If-None-Match and If-Modified-Since headers taken from the first response,
// and asserts those headers are present on the second HEAD request.
func TestHeadUsesConditionalHeaders(t *testing.T) {
	// User perspective: repeat sync should use cache validators.
	// System perspective: previous ETag/Last-Modified become HEAD conditionals.
	// Code perspective: If-None-Match and If-Modified-Since headers must be set.
	dir := t.TempDir()
	out := filepath.Join(dir, "state.txt")

	firstHead := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Etag": []string{"v1"}, "Last-Modified": []string{"Wed, 21 Oct 2015 07:28:00 GMT"}}, Body: io.NopCloser(strings.NewReader(""))}
	firstGet := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Etag": []string{"v1"}, "Last-Modified": []string{"Wed, 21 Oct 2015 07:28:00 GMT"}}, Body: io.NopCloser(strings.NewReader("abc"))}
	secondHead := &http.Response{StatusCode: http.StatusNotModified, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(""))}
	fc := &fakeClient{queue: []response{{resp: firstHead}, {resp: firstGet}, {resp: secondHead}}}

	s := &Syncer{Client: fc, Resource: "https://example.invalid/resource", OutputPath: out}
	if err := s.Sync(context.Background()); err != nil {
		t.Fatalf("first Sync() error = %v", err)
	}
	if err := s.Sync(context.Background()); err != nil {
		t.Fatalf("second Sync() error = %v", err)
	}

	if got := fc.headers[2].Get("If-None-Match"); got != "v1" {
		t.Fatalf("If-None-Match = %q, want %q", got, "v1")
	}
	if got := fc.headers[2].Get("If-Modified-Since"); got != "Wed, 21 Oct 2015 07:28:00 GMT" {
		t.Fatalf("If-Modified-Since = %q", got)
	}
}
