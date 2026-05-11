package httpclient

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// ---- helpers ----------------------------------------------------------------

type stubTransport struct {
	fn func(*http.Request) (*http.Response, error)
}

func (s *stubTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return s.fn(req)
}

func (s *stubTransport) Close() error { return nil }

func okResp(body string, headers map[string]string) *http.Response {
	r := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	return r
}

func newCache(now func() time.Time) *altSvcCache {
	c := newAltSvcCache()
	c.now = now
	return c
}

// ---- parseAltSvc ------------------------------------------------------------

// TestParseAltSvc verifies that users benefit from automatic HTTP/3 upgrades
// when servers advertise Alt-Svc in their response headers.
//
// This test covers the Alt-Svc header parsing logic in the httpclient package,
// which extracts h3 port and max-age for cache population.
//
// Table-driven cases cover: basic h3 token, ma parameter, missing ma,
// non-h3 tokens, malformed values, and multiple directives.
func TestParseAltSvc(t *testing.T) {
	cases := []struct {
		name     string
		header   string
		wantOK   bool
		wantPort string
		wantMA   time.Duration
	}{
		{"basic h3 with ma", `h3=":443"; ma=86400`, true, "443", 86400 * time.Second},
		{"h3 no ma uses default", `h3=":443"`, true, "443", altSvcDefaultMaxAge},
		{"h3 non-standard port", `h3=":8443"; ma=3600`, true, "8443", 3600 * time.Second},
		{"not h3", `h2=":443"; ma=3600`, false, "", 0},
		{"multi directive h3 second", `h2=":443", h3=":443"; ma=600`, true, "443", 600 * time.Second},
		{"h3 invalid port", `h3=":notaport"`, false, "", 0},
		{"h3 missing port", `h3=":"; ma=60`, false, "", 0},
		{"h3 zero port", `h3=":0"; ma=60`, false, "", 0},
		{"empty header", ``, false, "", 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			port, maxAge, ok := parseAltSvc(tc.header)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (port=%q)", ok, tc.wantOK, port)
			}
			if !tc.wantOK {
				return
			}
			if port != tc.wantPort {
				t.Fatalf("port = %q, want %q", port, tc.wantPort)
			}
			if maxAge != tc.wantMA {
				t.Fatalf("maxAge = %v, want %v", maxAge, tc.wantMA)
			}
		})
	}
}

// ---- altSvcCache ------------------------------------------------------------

// TestAltSvcCacheStorAndUsable verifies that cached Alt-Svc entries are
// correctly recognised as usable or expired, so users benefit from H3 upgrades
// only while the advertisement is valid.
//
// This test covers the altSvcCache type in the httpclient package.
//
// It stores an entry and checks usability before and after expiry by advancing
// a fake clock.
func TestAltSvcCacheStoreAndUsable(t *testing.T) {
	now := time.Unix(0, 0)
	c := newCache(func() time.Time { return now })

	const origin = "https://example.com:443"
	if c.usable(origin) {
		t.Fatal("usable before store, want false")
	}

	c.store(origin, "443", 10*time.Second)
	if !c.usable(origin) {
		t.Fatal("not usable right after store, want true")
	}

	now = now.Add(11 * time.Second)
	if c.usable(origin) {
		t.Fatal("still usable after expiry, want false")
	}
}

// TestAltSvcCacheCooldown verifies that after an H3 failure the origin is
// excluded from QUIC attempts for the cooldown period, protecting users from
// repeated failures on UDP-blocked networks.
//
// This test covers altSvcCache.setCooldown and usable in the httpclient package.
//
// It stores a valid entry, applies a cooldown, and asserts that the entry is
// not usable until the cooldown period has elapsed.
func TestAltSvcCacheCooldown(t *testing.T) {
	now := time.Unix(1_000, 0)
	c := newCache(func() time.Time { return now })

	const origin = "https://example.com:443"
	c.store(origin, "443", time.Hour)
	c.setCooldown(origin)

	if c.usable(origin) {
		t.Fatal("usable during cooldown, want false")
	}

	now = now.Add(altSvcH3Cooldown + time.Second)
	if !c.usable(origin) {
		t.Fatal("not usable after cooldown elapsed, want true")
	}
}

// TestAltSvcCacheCooldownWithoutEntry verifies that a cooldown can be set even
// when no cache entry exists (e.g. if the entry already expired), preventing a
// fresh H3 attempt immediately after a recent failure.
//
// This test covers altSvcCache.setCooldown on an absent entry in the httpclient
// package.
//
// It calls setCooldown on an unknown origin and asserts that a subsequent store
// followed by usable still returns false during the cooldown window.
func TestAltSvcCacheCooldownWithoutEntry(t *testing.T) {
	now := time.Unix(1_000, 0)
	c := newCache(func() time.Time { return now })

	const origin = "https://example.com:443"
	c.setCooldown(origin) // no prior entry
	c.store(origin, "443", time.Hour)

	if c.usable(origin) {
		t.Fatal("usable during cooldown on fresh entry, want false")
	}
}

// ---- altSvcTransport --------------------------------------------------------

// TestAltSvcTransportUpgradesOnCacheHit verifies that once an Alt-Svc entry is
// cached the transport automatically uses HTTP/3, giving users faster, more
// efficient connections without configuration.
//
// This test covers altSvcTransport.RoundTrip in the httpclient package.
//
// It pre-populates the cache, issues a request, and asserts the H3 transport
// was used (TCP transport not called).
func TestAltSvcTransportUpgradesOnCacheHit(t *testing.T) {
	tcpCalls, h3Calls := 0, 0
	now := time.Now()
	cache := newCache(func() time.Time { return now })
	cache.store("https://example.com:443", "443", time.Hour)

	tr := &altSvcTransport{
		cache: cache,
		tcp: &stubTransport{fn: func(_ *http.Request) (*http.Response, error) {
			tcpCalls++
			return okResp("tcp", nil), nil
		}},
		h3: &stubTransport{fn: func(_ *http.Request) (*http.Response, error) {
			h3Calls++
			return okResp("h3", nil), nil
		}},
	}

	req, _ := http.NewRequest(http.MethodGet, "https://example.com/path", nil)
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip() error = %v", err)
	}
	_ = resp.Body.Close()
	if h3Calls != 1 || tcpCalls != 0 {
		t.Fatalf("h3=%d tcp=%d, want h3=1 tcp=0", h3Calls, tcpCalls)
	}
}

// TestAltSvcTransportFallsBackOnH3Failure verifies that a broken QUIC path does
// not stop users from syncing their file — the transport falls back to TCP and
// records a cooldown so QUIC is not retried immediately.
//
// This test covers altSvcTransport.RoundTrip H3-failure path in the httpclient
// package.
//
// It pre-populates the cache, makes the H3 transport fail, and asserts TCP is
// used as a fallback and the cache entry is now in cooldown.
func TestAltSvcTransportFallsBackOnH3Failure(t *testing.T) {
	now := time.Now()
	cache := newCache(func() time.Time { return now })
	cache.store("https://example.com:443", "443", time.Hour)

	tcpCalls := 0
	tr := &altSvcTransport{
		cache: cache,
		tcp: &stubTransport{fn: func(_ *http.Request) (*http.Response, error) {
			tcpCalls++
			return okResp("tcp", nil), nil
		}},
		h3: &stubTransport{fn: func(_ *http.Request) (*http.Response, error) {
			return nil, errors.New("quic failed")
		}},
	}

	req, _ := http.NewRequest(http.MethodGet, "https://example.com/path", nil)
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip() error = %v", err)
	}
	_ = resp.Body.Close()
	if tcpCalls != 1 {
		t.Fatalf("tcp calls = %d, want 1", tcpCalls)
	}
	// Confirm cooldown prevents immediate H3 retry.
	if cache.usable("https://example.com:443") {
		t.Fatal("cache still usable after H3 failure, want cooldown")
	}
}

// TestAltSvcTransportPopulatesCacheFromTCPResponse verifies that a server
// advertising h3 in Alt-Svc causes the next request to be made over HTTP/3,
// so users benefit automatically from the upgrade without any configuration.
//
// This test covers the Alt-Svc parsing and cache-population path of
// altSvcTransport.RoundTrip in the httpclient package.
//
// It issues two requests: the first via TCP receives an Alt-Svc header;
// the second asserts that the H3 transport is used.
func TestAltSvcTransportPopulatesCacheFromTCPResponse(t *testing.T) {
	now := time.Now()
	cache := newCache(func() time.Time { return now })

	h3Calls, tcpCalls := 0, 0
	tr := &altSvcTransport{
		cache: cache,
		tcp: &stubTransport{fn: func(_ *http.Request) (*http.Response, error) {
			tcpCalls++
			return okResp("tcp", map[string]string{"Alt-Svc": `h3=":443"; ma=3600`}), nil
		}},
		h3: &stubTransport{fn: func(_ *http.Request) (*http.Response, error) {
			h3Calls++
			return okResp("h3", nil), nil
		}},
	}

	req1, _ := http.NewRequest(http.MethodGet, "https://example.com/path", nil)
	resp1, err := tr.RoundTrip(req1)
	if err != nil {
		t.Fatalf("first RoundTrip() error = %v", err)
	}
	_ = resp1.Body.Close()

	req2, _ := http.NewRequest(http.MethodGet, "https://example.com/path", nil)
	resp2, err := tr.RoundTrip(req2)
	if err != nil {
		t.Fatalf("second RoundTrip() error = %v", err)
	}
	_ = resp2.Body.Close()

	if tcpCalls != 1 || h3Calls != 1 {
		t.Fatalf("tcp=%d h3=%d, want tcp=1 h3=1", tcpCalls, h3Calls)
	}
}

// TestAltSvcTransportNoUpgradeWhenNoAltSvc verifies that when a server does not
// advertise Alt-Svc, all requests continue over TCP without unexpected failures.
//
// This test covers the no-advertisement path of altSvcTransport.RoundTrip.
//
// It issues two requests to a server that returns no Alt-Svc and asserts that
// both are served by the TCP transport and the H3 transport is never called.
func TestAltSvcTransportNoUpgradeWhenNoAltSvc(t *testing.T) {
	now := time.Now()
	cache := newCache(func() time.Time { return now })
	h3Calls, tcpCalls := 0, 0

	tr := &altSvcTransport{
		cache: cache,
		tcp: &stubTransport{fn: func(_ *http.Request) (*http.Response, error) {
			tcpCalls++
			return okResp("tcp", nil), nil
		}},
		h3: &stubTransport{fn: func(_ *http.Request) (*http.Response, error) {
			h3Calls++
			return okResp("h3", nil), nil
		}},
	}

	for i := 0; i < 2; i++ {
		req, _ := http.NewRequest(http.MethodGet, "https://example.com/path", nil)
		resp, err := tr.RoundTrip(req)
		if err != nil {
			t.Fatalf("RoundTrip() error = %v", err)
		}
		_ = resp.Body.Close()
	}
	if h3Calls != 0 || tcpCalls != 2 {
		t.Fatalf("h3=%d tcp=%d, want h3=0 tcp=2", h3Calls, tcpCalls)
	}
}

// ---- httpclient.New ---------------------------------------------------------

// TestNewReturnsStdClientWhenHTTP3Disabled verifies that the default runtime
// works without HTTP/3 and that no extra resources need to be cleaned up.
//
// This test covers the New constructor in the httpclient package.
//
// It calls New with enableHTTP3=false and asserts that the returned client is a
// plain *http.Client and that the CloseFunc completes without error.
func TestNewReturnsStdClientWhenHTTP3Disabled(t *testing.T) {
	client, closeFn := New(2*time.Second, false)
	if _, ok := client.(*http.Client); !ok {
		t.Fatalf("expected standard http.Client when HTTP/3 disabled")
	}
	if err := closeFn(); err != nil {
		t.Fatalf("closeFn() error = %v", err)
	}
}

// TestNewReturnsAltSvcClientWhenHTTP3Enabled verifies that enabling HTTP/3
// produces a client backed by the altSvcTransport, ready for auto-upgrade.
//
// This test covers the New constructor in the httpclient package.
//
// It calls New with enableHTTP3=true and asserts that the returned client is an
// *http.Client with an altSvcTransport and that the CloseFunc is callable.
func TestNewReturnsAltSvcClientWhenHTTP3Enabled(t *testing.T) {
	client, closeFn := New(2*time.Second, true)
	defer func() { _ = closeFn() }()

	hc, ok := client.(*http.Client)
	if !ok {
		t.Fatalf("expected *http.Client, got %T", client)
	}
	if _, ok2 := hc.Transport.(*altSvcTransport); !ok2 {
		t.Fatalf("expected altSvcTransport, got %T", hc.Transport)
	}
}
