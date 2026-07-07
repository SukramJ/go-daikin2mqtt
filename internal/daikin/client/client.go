// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

// Package client implements the ONECTA Cloud HTTP transport used to read
// gateway devices and patch their characteristics. It encapsulates the
// quirks of the Daikin ONECTA cloud API: a global cloud lock that allows
// only one in-flight request, a post-write "scan ignore" window during
// which the cloud returns stale data, rate-limit accounting, GET retries
// with exponential backoff and jitter, and a circuit breaker that trips on
// server/network errors (but not on rate limiting).
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultBaseURL is the production ONECTA cloud API base URL.
const DefaultBaseURL = "https://api.onecta.daikineurope.com"

// Circuit breaker tuning constants.
const (
	cbFailureThreshold = 5
	cbRecoveryTimeout  = 60 * time.Second
)

// Sentinel errors returned by the client. Callers can match them with
// [errors.Is].
var (
	// ErrScanIgnore is returned by [Client.GetDevices] when a read is
	// attempted within the scan-ignore window after a successful PATCH,
	// because the cloud would return stale data.
	ErrScanIgnore = errors.New("client: scan ignore window active")
	// ErrRateLimited is returned when the cloud responds with HTTP 429.
	ErrRateLimited = errors.New("client: rate limited")
	// ErrCircuitOpen is returned when the circuit breaker is open and the
	// request is short-circuited before hitting the network.
	ErrCircuitOpen = errors.New("client: circuit breaker open")
)

// sleep is overridable in tests so backoff delays don't slow them down.
var sleep = time.Sleep

// TokenProvider yields a valid bearer access token. It is implemented by
// auth.TokenSource.
type TokenProvider interface {
	Token(ctx context.Context) (string, error)
}

// tokenInvalidator is an optional capability: when the cloud rejects a token
// with HTTP 401, the client invalidates it so the next request refreshes.
// *auth.TokenSource implements this.
type tokenInvalidator interface {
	Invalidate()
}

// Options configures a [Client]. Only Tokens is required; the rest have
// sensible defaults applied by [New].
type Options struct {
	// BaseURL is the API base URL; defaults to [DefaultBaseURL].
	BaseURL string
	// Tokens provides bearer access tokens; required.
	Tokens TokenProvider
	// HTTPClient is the underlying HTTP client; defaults to a client with a
	// 60s timeout.
	HTTPClient *http.Client
	// Logger is used for diagnostics; defaults to [slog.Default].
	Logger *slog.Logger
	// ScanIgnore is the window after a PATCH during which GETs are blocked;
	// defaults to 30s.
	ScanIgnore time.Duration
	// Clock returns the current time; defaults to [time.Now]. Injectable for
	// tests covering scan-ignore, rate-limit reset, and breaker timing.
	Clock func() time.Time
	// RetryBaseDelay is the base delay for GET retry backoff; defaults to 1s.
	RetryBaseDelay time.Duration
	// RetryAttempts is the total number of GET attempts; defaults to 3.
	RetryAttempts int
	// MockExampleID, when set, routes GETs to the ONECTA mock endpoint
	// (/mock/v1/gateway-devices) and sends the X-Mocking-Example-Id header so
	// developers can test against fixed example devices without owning one.
	MockExampleID string
}

// RateLimit is a snapshot of the rate-limit accounting parsed from the most
// recent response headers.
type RateLimit struct {
	LimitMinute, RemainingMinute int
	LimitDay, RemainingDay       int
	RetryAfter                   int       // seconds, from retry-after on 429
	ResetAt                      time.Time // from ratelimit-reset if present
	Updated                      time.Time
}

// breakerState models the circuit breaker lifecycle.
type breakerState int

const (
	stateClosed breakerState = iota
	stateOpen
	stateHalfOpen
)

// Client is a thread-safe ONECTA cloud transport. The cloud only tolerates
// a single in-flight request, so every request is serialized through a
// mutex.
type Client struct {
	baseURL    string
	tokens     TokenProvider
	httpClient *http.Client
	logger     *slog.Logger
	scanIgnore time.Duration
	clock      func() time.Time
	retryBase  time.Duration
	retryMax   int
	mockID     string

	// cloudLock serializes the whole request (token fetch + HTTP do +
	// header parsing) to honor the single-in-flight cloud constraint.
	cloudLock sync.Mutex

	// mu guards the mutable state below.
	mu        sync.Mutex
	lastPatch time.Time
	rateLimit RateLimit

	// circuit breaker state, guarded by mu.
	cbState    breakerState
	cbFailures int
	cbOpenedAt time.Time
}

// New builds a [Client], applying defaults for any unset [Options] fields.
func New(o Options) *Client {
	c := &Client{
		baseURL:    o.BaseURL,
		tokens:     o.Tokens,
		httpClient: o.HTTPClient,
		logger:     o.Logger,
		scanIgnore: o.ScanIgnore,
		clock:      o.Clock,
		retryBase:  o.RetryBaseDelay,
		retryMax:   o.RetryAttempts,
		mockID:     o.MockExampleID,
	}
	if c.baseURL == "" {
		c.baseURL = DefaultBaseURL
	}
	if c.httpClient == nil {
		// Defense in depth: every request holds cloudLock, so a stalled peer
		// must never hang a request (and the whole daemon) forever.
		c.httpClient = &http.Client{Timeout: 60 * time.Second}
	}
	if c.logger == nil {
		c.logger = slog.Default()
	}
	if c.scanIgnore == 0 {
		c.scanIgnore = 30 * time.Second
	}
	if c.clock == nil {
		c.clock = time.Now
	}
	if c.retryBase == 0 {
		c.retryBase = time.Second
	}
	if c.retryMax == 0 {
		c.retryMax = 3
	}
	return c
}

// invalidateToken forces a token refresh on the next request if supported.
func (c *Client) invalidateToken() {
	if inv, ok := c.tokens.(tokenInvalidator); ok {
		inv.Invalidate()
	}
}

// RateLimit returns a copy of the most recent rate-limit snapshot.
func (c *Client) RateLimit() RateLimit {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.rateLimit
}

// GetDevices performs GET /v1/gateway-devices and returns the raw JSON body.
//
// It returns [ErrScanIgnore] (and a nil body) when called within ScanIgnore
// after a successful PATCH, [ErrRateLimited] on HTTP 429, and [ErrCircuitOpen]
// when the breaker is open. Transient failures (5xx / network) are retried
// with exponential backoff plus jitter up to RetryAttempts.
func (c *Client) GetDevices(ctx context.Context) (json.RawMessage, error) {
	c.mu.Lock()
	withinIgnore := !c.lastPatch.IsZero() && c.clock().Sub(c.lastPatch) < c.scanIgnore
	c.mu.Unlock()
	if withinIgnore {
		c.logger.Debug("skipping GET within scan-ignore window")
		return nil, ErrScanIgnore
	}

	c.cloudLock.Lock()
	defer c.cloudLock.Unlock()

	if err := c.checkBreaker(); err != nil {
		return nil, err
	}

	reqURL := c.baseURL + "/v1/gateway-devices"
	if c.mockID != "" {
		reqURL = c.baseURL + "/mock/v1/gateway-devices"
	}

	var lastErr error
	refreshedOn401 := false
	for attempt := range c.retryMax {
		if attempt > 0 {
			d := c.backoff(attempt)
			c.logger.Debug("retrying GET after backoff", "attempt", attempt, "delay", d)
			if err := c.wait(ctx, d); err != nil {
				return nil, err
			}
		}

		body, status, retryable, err := c.doGet(ctx, reqURL)
		if err != nil {
			lastErr = err
			if retryable {
				c.recordFailure()
				continue
			}
			// Non-retryable transport/token error: do not count toward the
			// breaker (it is not a server fault).
			return nil, err
		}

		switch {
		case status == http.StatusOK:
			c.recordSuccess()
			return json.RawMessage(body), nil
		case status == http.StatusTooManyRequests:
			// Rate limiting is not a circuit-breaker failure.
			return nil, fmt.Errorf("%w: GET gateway-devices", ErrRateLimited)
		case status == http.StatusUnauthorized:
			// A 401 can mean the cached token was invalidated server-side
			// (Daikin rotates refresh tokens). Force a refresh and retry once;
			// not a circuit-breaker failure.
			if !refreshedOn401 {
				refreshedOn401 = true
				c.invalidateToken()
				c.logger.Debug("got 401 on GET; forcing token refresh and retrying")
				lastErr = errors.New("client: GET gateway-devices: status 401")
				continue
			}
			return nil, errors.New("client: GET gateway-devices: status 401 (re-authentication may be required)")
		case status >= 500:
			lastErr = fmt.Errorf("client: GET gateway-devices: status %d", status)
			c.recordFailure()
			continue
		default:
			return nil, fmt.Errorf("client: GET gateway-devices: status %d", status)
		}
	}

	if lastErr == nil {
		lastErr = errors.New("client: GET gateway-devices: exhausted retries")
	}
	return nil, lastErr
}

// Patch performs PATCH on the given characteristic data point. value is
// JSON-marshalled into {"value":value}; when path is non-empty, "path":path
// is added. Success is HTTP 204; on success the scan-ignore window starts.
func (c *Client) Patch(ctx context.Context, deviceID, embeddedID, characteristic string, value any, path string) error {
	payload := map[string]any{"value": value}
	if path != "" {
		payload["path"] = path
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("client: marshal patch body: %w", err)
	}

	// The IDs come from MQTT topic segments / the web API; escape them so they
	// cannot inject path segments or query/fragment parts into the URL.
	reqURL := fmt.Sprintf("%s/v1/gateway-devices/%s/management-points/%s/characteristics/%s",
		c.baseURL, url.PathEscape(deviceID), url.PathEscape(embeddedID), url.PathEscape(characteristic))

	c.cloudLock.Lock()
	defer c.cloudLock.Unlock()

	if err := c.checkBreaker(); err != nil {
		return err
	}

	// Up to two attempts: a 401 forces a token refresh and one retry.
	for attempt := range 2 {
		token, err := c.tokens.Token(ctx)
		if err != nil {
			return err
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPatch, reqURL, bytes.NewReader(body))
		if err != nil {
			return fmt.Errorf("client: build patch request: %w", err)
		}
		c.setCommonHeaders(req, token)
		req.Header.Set("Content-Type", "application/json")

		resp, err := c.httpClient.Do(req)
		if err != nil {
			c.recordFailure()
			return fmt.Errorf("client: patch request: %w", err)
		}
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
		c.parseRateLimit(resp.Header)

		switch {
		case resp.StatusCode == http.StatusNoContent:
			c.recordSuccess()
			c.mu.Lock()
			c.lastPatch = c.clock()
			c.mu.Unlock()
			return nil
		case resp.StatusCode == http.StatusTooManyRequests:
			return fmt.Errorf("%w: PATCH %s", ErrRateLimited, characteristic)
		case resp.StatusCode == http.StatusUnauthorized && attempt == 0:
			c.invalidateToken()
			c.logger.Debug("got 401 on PATCH; forcing token refresh and retrying")
			continue
		case resp.StatusCode >= 500:
			c.recordFailure()
			return fmt.Errorf("client: PATCH %s: status %d: %s", characteristic, resp.StatusCode, snippet(respBody))
		default:
			return fmt.Errorf("client: PATCH %s: status %d: %s", characteristic, resp.StatusCode, snippet(respBody))
		}
	}
	return fmt.Errorf("client: PATCH %s: unauthorized after token refresh", characteristic)
}

// doGet executes a single GET attempt. It returns the body, HTTP status,
// whether the error is retryable, and any transport/token error.
func (c *Client) doGet(ctx context.Context, reqURL string) (body []byte, status int, retryable bool, err error) {
	token, err := c.tokens.Token(ctx)
	if err != nil {
		// Token errors (e.g. auth.ErrReauthRequired) are propagated
		// unchanged and are not retryable.
		return nil, 0, false, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, http.NoBody)
	if err != nil {
		return nil, 0, false, fmt.Errorf("client: build get request: %w", err)
	}
	c.setCommonHeaders(req, token)
	if c.mockID != "" {
		req.Header.Set("X-Mocking-Example-Id", c.mockID)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		// Network errors are retryable.
		return nil, 0, true, fmt.Errorf("client: get request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	c.parseRateLimit(resp.Header)

	b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, resp.StatusCode, true, fmt.Errorf("client: read get response: %w", err)
	}
	return b, resp.StatusCode, false, nil
}

// setCommonHeaders applies the headers required on every cloud request.
//
// Note: we deliberately do NOT set Accept-Encoding ourselves. Go's HTTP
// transport adds "Accept-Encoding: gzip" automatically and transparently
// decompresses the response — but only when it added the header itself.
// Setting it manually would suppress that auto-decompression and leak raw
// gzip bytes to the caller.
func (c *Client) setCommonHeaders(req *http.Request, token string) {
	req.Header.Set("Authorization", "Bearer "+token)
}

// backoff returns the delay before the given (1-based) retry attempt:
// retryBase * 2^(attempt-1) plus up to 50% jitter.
func (c *Client) backoff(attempt int) time.Duration {
	base := c.retryBase << (attempt - 1)
	jitter := time.Duration(rand.Int63n(int64(base)/2 + 1)) //nolint:gosec // jitter only, no security relevance
	return base + jitter
}

// wait sleeps for d, honoring context cancellation.
func (c *Client) wait(ctx context.Context, d time.Duration) error {
	done := make(chan struct{})
	go func() {
		sleep(d)
		close(done)
	}()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-done:
		return nil
	}
}

// parseRateLimit reads rate-limit headers and stores a snapshot.
func (c *Client) parseRateLimit(h http.Header) {
	rl := RateLimit{
		LimitMinute:     atoiHeader(h, "X-RateLimit-Limit-minute"),
		RemainingMinute: atoiHeader(h, "X-RateLimit-Remaining-minute"),
		LimitDay:        atoiHeader(h, "X-RateLimit-Limit-day"),
		RemainingDay:    atoiHeader(h, "X-RateLimit-Remaining-day"),
		RetryAfter:      atoiHeader(h, "retry-after"),
		Updated:         c.clock(),
	}
	if v := h.Get("ratelimit-reset"); v != "" {
		if secs, err := strconv.Atoi(v); err == nil {
			rl.ResetAt = c.clock().Add(time.Duration(secs) * time.Second)
		}
	}

	c.mu.Lock()
	c.rateLimit = rl
	c.mu.Unlock()
}

// atoiHeader parses an integer header, returning 0 when absent or invalid.
func atoiHeader(h http.Header, key string) int {
	v := h.Get(key)
	if v == "" {
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0
	}
	return n
}

// checkBreaker short-circuits when the breaker is open and not yet eligible
// for recovery. Must be called while holding cloudLock; it manages its own
// mu locking.
func (c *Client) checkBreaker() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.cbState == stateOpen {
		if c.clock().Sub(c.cbOpenedAt) >= cbRecoveryTimeout {
			c.cbState = stateHalfOpen
			c.logger.Debug("circuit breaker half-open")
			return nil
		}
		return ErrCircuitOpen
	}
	return nil
}

// recordSuccess resets the breaker to closed.
func (c *Client) recordSuccess() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cbFailures = 0
	if c.cbState != stateClosed {
		c.logger.Debug("circuit breaker closed")
	}
	c.cbState = stateClosed
}

// recordFailure increments the failure counter and trips the breaker when
// the threshold is reached (or immediately when half-open).
func (c *Client) recordFailure() {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.cbState == stateHalfOpen {
		c.cbState = stateOpen
		c.cbOpenedAt = c.clock()
		c.logger.Warn("circuit breaker re-opened from half-open")
		return
	}

	c.cbFailures++
	if c.cbFailures >= cbFailureThreshold {
		c.cbState = stateOpen
		c.cbOpenedAt = c.clock()
		c.logger.Warn("circuit breaker opened", "failures", c.cbFailures)
	}
}

// snippet trims and caps a response body for inclusion in error messages.
func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	const maxLen = 300
	if len(s) > maxLen {
		return s[:maxLen] + "…"
	}
	return s
}
