package httpclient

import (
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// altSvcDefaultMaxAge is the TTL used when Alt-Svc carries no ma parameter.
	altSvcDefaultMaxAge = 24 * time.Hour
	// altSvcH3Cooldown is the period after a failed HTTP/3 attempt during which
	// the origin is not retried over QUIC, so UDP-blocked networks are not punished.
	altSvcH3Cooldown = 7 * time.Minute
	// altSvcMaxMaSecs is the maximum accepted value for the Alt-Svc ma= parameter.
	// Values above this are clamped to prevent time.Duration overflow when the
	// seconds value is multiplied by time.Second (1e9 ns): any secs > MaxInt64/1e9
	// would wrap to a negative duration and make cache entries expire immediately.
	// 7 days is a generous upper bound; the spec does not define a maximum.
	altSvcMaxMaSecs = int64(7 * 24 * time.Hour / time.Second) // 604800
)

// h3Entry describes a cached HTTP/3 advertisement for a single origin.
type h3Entry struct {
	// expiresAt is when the Alt-Svc advertisement lapses.
	expiresAt time.Time
	// cooldownUntil is non-zero when a recent H3 attempt failed; no H3 retries
	// are made until this instant passes.
	cooldownUntil time.Time
	// altPort is the advertised QUIC port string (e.g. "443").
	altPort string
}

// altSvcCache stores per-origin HTTP/3 Alt-Svc advertisements.
// All methods are safe for concurrent use.
type altSvcCache struct {
	mu      sync.Mutex
	entries map[string]*h3Entry // key: origin string ("https://host:port")
	now     func() time.Time    // injectable for testing
}

func newAltSvcCache() *altSvcCache {
	return &altSvcCache{
		entries: make(map[string]*h3Entry),
		now:     time.Now,
	}
}

// origin returns the canonical origin key for a request ("scheme://host:port").
func originKey(req *http.Request) string {
	host := req.URL.Host
	if req.URL.Port() == "" {
		if req.URL.Scheme == "https" {
			host += ":443"
		} else {
			host += ":80"
		}
	}
	return req.URL.Scheme + "://" + host
}

// usable reports whether the origin has a valid, non-cooled-down H3 cache entry.
func (c *altSvcCache) usable(origin string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[origin]
	if !ok {
		return false
	}
	now := c.now()
	if now.After(e.expiresAt) {
		delete(c.entries, origin)
		return false
	}
	if !e.cooldownUntil.IsZero() && now.Before(e.cooldownUntil) {
		return false
	}
	return true
}

// store saves an H3 advertisement for the origin.
func (c *altSvcCache) store(origin, altPort string, maxAge time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if maxAge <= 0 {
		maxAge = altSvcDefaultMaxAge
	}
	// Preserve any active cooldown from a prior failed H3 attempt.
	var cooldown time.Time
	if prev, ok := c.entries[origin]; ok {
		cooldown = prev.cooldownUntil
	}
	c.entries[origin] = &h3Entry{
		expiresAt:     c.now().Add(maxAge),
		altPort:       altPort,
		cooldownUntil: cooldown,
	}
}

// setCooldown marks the origin as temporarily unavailable over H3.
func (c *altSvcCache) setCooldown(origin string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[origin]
	if !ok {
		// Create a placeholder so we track cooldown even without a stored entry.
		e = &h3Entry{}
		c.entries[origin] = e
	}
	e.cooldownUntil = c.now().Add(altSvcH3Cooldown)
	// If this was a placeholder with no real expiry, set expiry to end of cooldown
	// so the entry is not immediately evicted by the next usable() call.
	if e.expiresAt.IsZero() {
		e.expiresAt = c.now().Add(altSvcH3Cooldown)
	}
}

// parseAltSvc extracts all "h3=":PORT"" tokens from an Alt-Svc header value
// and returns (port, maxAge, ok). The header may contain multiple
// comma-separated directives. Only the first h3 token is returned.
func parseAltSvc(headerVal string) (string, time.Duration, bool) {
	defaultMaxAge := altSvcDefaultMaxAge
	for directive := range splitDirectives(headerVal) {
		directive = strings.TrimSpace(directive)
		params := strings.Split(directive, ";")
		if len(params) == 0 {
			continue
		}
		proto := strings.TrimSpace(params[0])
		if !strings.HasPrefix(proto, `h3="`) {
			continue
		}
		// proto looks like: h3=":PORT"
		portPart := strings.TrimPrefix(proto, `h3="`)
		portPart = strings.TrimSuffix(portPart, `"`)
		portPart = strings.TrimPrefix(portPart, ":")
		if portPart == "" {
			continue
		}
		if p, err := strconv.Atoi(portPart); err != nil || p <= 0 || p > 65535 {
			continue
		}
		maxAge := defaultMaxAge
		// Parse optional ma= parameter.
		for _, param := range params[1:] {
			param = strings.TrimSpace(param)
			if strings.HasPrefix(param, "ma=") {
				if secs, err := strconv.ParseInt(strings.TrimPrefix(param, "ma="), 10, 64); err == nil && secs > 0 {
					if secs > altSvcMaxMaSecs {
						secs = altSvcMaxMaSecs
					}
					maxAge = time.Duration(secs) * time.Second
				}
			}
		}
		return portPart, maxAge, true
	}
	return "", 0, false
}

// splitDirectives is a range-over-func iterator over comma-separated Alt-Svc
// directives, respecting quoted strings so commas inside quotes are not splits.
func splitDirectives(s string) func(func(string) bool) {
	return func(yield func(string) bool) {
		start := 0
		inQuote := false
		for i := 0; i < len(s); i++ {
			switch s[i] {
			case '"':
				inQuote = !inQuote
			case ',':
				if !inQuote {
					if !yield(s[start:i]) {
						return
					}
					start = i + 1
				}
			}
		}
		yield(s[start:])
	}
}

// h3RoundTripper is the minimal interface required for HTTP/3 requests.
// *http3.Transport satisfies this interface.
type h3RoundTripper interface {
	RoundTrip(req *http.Request) (*http.Response, error)
	Close() error
}

// altSvcTransport is an http.RoundTripper that performs Alt-Svc-based HTTP/3
// auto-upgrade.
//
// On every request it consults an in-memory cache keyed by origin
// (scheme+host+port). If the cache holds a valid, non-cooled-down H3
// advertisement the request is first attempted over QUIC; on any H3 failure
// a cooldown is recorded and the request falls back to the TCP transport.
//
// When no H3 cache entry exists the TCP transport is used, and the Alt-Svc
// response header is parsed to populate the cache for subsequent requests.
//
// The cache is safe for concurrent use across goroutines.
type altSvcTransport struct {
	tcp   http.RoundTripper
	h3    h3RoundTripper
	cache *altSvcCache
}

// RoundTrip implements http.RoundTripper.
//
// req is the outgoing HTTP request; it must have a non-nil URL with a valid
// scheme and host. The method decides per-origin whether to attempt HTTP/3
// first (cache hit with no active cooldown) or fall back directly to TCP
// (cache miss or active cooldown). On an H3 failure the origin's cooldown is
// set and the request is retried over TCP (provided no body has been consumed).
//
// Alt-Svc headers from TCP responses are parsed and stored in the cache so
// that subsequent requests to the same origin automatically upgrade.
func (t *altSvcTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	origin := originKey(req)

	if t.cache.usable(origin) {
		resp, err := t.h3.RoundTrip(req)
		if err == nil {
			return resp, nil
		}
		// H3 failed — record cooldown, retry over TCP (body-less only).
		t.cache.setCooldown(origin)
		if req.Body != nil {
			return nil, err
		}
		cloned := req.Clone(req.Context())
		return t.tcp.RoundTrip(cloned)
	}

	// No usable H3 entry: use TCP and check Alt-Svc response header.
	resp, err := t.tcp.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	if altSvcHeader := resp.Header.Get("Alt-Svc"); altSvcHeader != "" {
		if port, maxAge, ok := parseAltSvc(altSvcHeader); ok {
			t.cache.store(origin, port, maxAge)
		}
	}
	return resp, nil
}
