package paprikaclient

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Defaults for [Config] fields left at their zero value.
const (
	DefaultBaseURL   = "https://www.paprikaapp.com"
	DefaultUserAgent = "paprika-mcp-go/dev"
	// DefaultRequestTimeout bounds one Paprika call. It must stay well below the
	// inbound request timeout (handler.DefaultRequestTimeout, 90 s): requests
	// are served one at a time, a call can wait behind another caller's hung
	// call, and a write makes several calls in sequence. Live testing saw
	// grocery calls hang for 30 s, so 20 s fails a hung call early and still
	// leaves room for a slow healthy one.
	DefaultRequestTimeout   = 20 * time.Second
	DefaultMinInterval      = 250 * time.Millisecond
	DefaultLoginBackoffMin  = time.Minute
	DefaultLoginBackoffMax  = time.Hour
	DefaultTimeoutThreshold = 3
	DefaultTimeoutPause     = 5 * time.Minute
	DefaultMaxResponseBytes = 32 << 20
)

const (
	loginPath  = "/api/v1/account/login/"
	syncPrefix = "/api/v2/sync/"
)

// Config configures a [Client]. Only Email and Password are required.
type Config struct {
	// BaseURL defaults to DefaultBaseURL. Plain HTTP is accepted only for
	// loopback hosts, which tests use.
	BaseURL string
	// Email and Password are the Paprika account credentials.
	Email    string
	Password string
	// UserAgent defaults to DefaultUserAgent. Callers should pass
	// "paprika-mcp-go/<version>".
	UserAgent string
	// HTTPClient is optional. The client uses a copy of it with redirects
	// disabled and its own Timeout cleared, so its Transport is the only part
	// that matters.
	HTTPClient *http.Client
	// Logger receives structured events. It defaults to slog.Default().
	Logger *slog.Logger

	// RequestTimeout bounds each request, including reading the body.
	RequestTimeout time.Duration
	// MinInterval is the minimum time from the end of one request to the start
	// of the next. A negative value disables pacing.
	MinInterval time.Duration
	// LoginBackoffMin and LoginBackoffMax bound the exponential wait after
	// rejected logins.
	LoginBackoffMin time.Duration
	LoginBackoffMax time.Duration
	// TimeoutThreshold is the number of consecutive timeouts that triggers a
	// pause of TimeoutPause.
	TimeoutThreshold int
	TimeoutPause     time.Duration
	// MaxResponseBytes caps a response after decompression.
	MaxResponseBytes int64
	// Now replaces time.Now for backoff and pause decisions. Tests use it.
	Now func() time.Time
}

// Client talks to the Paprika API. It is safe for concurrent use; requests are
// serialized and paced internally.
type Client struct {
	cfg  Config
	base string
	http *http.Client
	log  *slog.Logger
	gate chan struct{}

	lastEnd time.Time // guarded by gate

	mu                sync.Mutex
	token             string
	loginCall         *loginCall
	loginFailures     int
	loginBlockedUntil time.Time
	consecutiveTO     int
	pausedUntil       time.Time
}

type loginCall struct {
	done  chan struct{}
	token string
	err   error
}

// New validates the configuration and returns a Client. It makes no requests.
func New(cfg Config) (*Client, error) {
	if cfg.Email == "" || cfg.Password == "" {
		return nil, fmt.Errorf("%w: email and password are required", ErrInvalidArgument)
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = DefaultBaseURL
	}
	base, err := validateBaseURL(cfg.BaseURL)
	if err != nil {
		return nil, err
	}
	if cfg.UserAgent == "" {
		cfg.UserAgent = DefaultUserAgent
	}
	if cfg.RequestTimeout <= 0 {
		cfg.RequestTimeout = DefaultRequestTimeout
	}
	if cfg.MinInterval == 0 {
		cfg.MinInterval = DefaultMinInterval
	}
	if cfg.LoginBackoffMin <= 0 {
		cfg.LoginBackoffMin = DefaultLoginBackoffMin
	}
	if cfg.LoginBackoffMax <= 0 {
		cfg.LoginBackoffMax = DefaultLoginBackoffMax
	}
	if cfg.LoginBackoffMax < cfg.LoginBackoffMin {
		cfg.LoginBackoffMax = cfg.LoginBackoffMin
	}
	if cfg.TimeoutThreshold <= 0 {
		cfg.TimeoutThreshold = DefaultTimeoutThreshold
	}
	if cfg.TimeoutPause <= 0 {
		cfg.TimeoutPause = DefaultTimeoutPause
	}
	if cfg.MaxResponseBytes <= 0 {
		cfg.MaxResponseBytes = DefaultMaxResponseBytes
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	httpClient := &http.Client{}
	if cfg.HTTPClient != nil {
		clone := *cfg.HTTPClient
		httpClient = &clone
	}
	httpClient.Timeout = 0
	httpClient.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}

	return &Client{
		cfg:  cfg,
		base: base,
		http: httpClient,
		log:  logger,
		gate: make(chan struct{}, 1),
	}, nil
}

func validateBaseURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("%w: invalid base URL", ErrInvalidArgument)
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !isLoopback(u.Hostname()) {
			return "", fmt.Errorf("%w: plain HTTP is only allowed for loopback hosts", ErrInvalidArgument)
		}
	default:
		return "", fmt.Errorf("%w: base URL scheme must be https", ErrInvalidArgument)
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("%w: base URL must be only scheme, host, and port", ErrInvalidArgument)
	}
	return strings.TrimRight(u.Scheme+"://"+u.Host+u.Path, "/"), nil
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	addr, err := netip.ParseAddr(host)
	return err == nil && addr.IsLoopback()
}

// request is one HTTP exchange.
type request struct {
	method      string
	path        string
	body        []byte
	contentType string
	token       string
	// unknownOnFail marks a write whose transport failure leaves the outcome
	// unknown.
	unknownOnFail bool
}

type response struct {
	status int
	body   []byte
}

// do runs a request with authentication, and repeats it once after a 401 and a
// fresh login. It returns the envelope's result.
func (c *Client) do(ctx context.Context, method, path string, body []byte, contentType string) (json.RawMessage, error) {
	req := request{
		method:        method,
		path:          path,
		body:          body,
		contentType:   contentType,
		unknownOnFail: method == http.MethodPost,
	}
	for attempt := 0; ; attempt++ {
		token, err := c.ensureToken(ctx)
		if err != nil {
			return nil, err
		}
		req.token = token
		res, err := c.send(ctx, req)
		if err != nil {
			return nil, err
		}
		if res.status == http.StatusUnauthorized {
			c.invalidate(token)
			if attempt == 0 {
				c.log.Info("paprika token refused, logging in again")
				continue
			}
			return nil, fmt.Errorf("%w: token refused after a fresh login", ErrUnauthorized)
		}
		return decodeEnvelope(res, req.unknownOnFail)
	}
}

func (c *Client) invalidate(token string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token == token {
		c.token = ""
	}
}

// ensureToken returns the current token, logging in if there is none. Callers
// share one login attempt, and failed attempts back off.
func (c *Client) ensureToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	if c.token != "" {
		token := c.token
		c.mu.Unlock()
		return token, nil
	}
	call := c.loginCall
	if call == nil {
		now := c.cfg.Now()
		if now.Before(c.loginBlockedUntil) {
			wait := c.loginBlockedUntil.Sub(now)
			c.mu.Unlock()
			return "", fmt.Errorf("%w: login failed recently, next attempt in %s", ErrUnauthorized, wait.Round(time.Second))
		}
		call = &loginCall{done: make(chan struct{})}
		c.loginCall = call
		go c.runLogin(call)
	}
	c.mu.Unlock()

	select {
	case <-call.done:
		return call.token, call.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func (c *Client) runLogin(call *loginCall) {
	token, err := c.login(context.Background())

	c.mu.Lock()
	switch {
	case err == nil:
		c.token = token
		c.loginFailures = 0
		c.loginBlockedUntil = time.Time{}
	case errors.Is(err, ErrUnauthorized):
		c.loginFailures++
		wait := c.backoff(c.loginFailures)
		c.loginBlockedUntil = c.cfg.Now().Add(wait)
		c.log.Warn("paprika login rejected", "failures", c.loginFailures, "retry_in", wait)
	}
	c.loginCall = nil
	call.token, call.err = token, err
	c.mu.Unlock()
	close(call.done)
}

func (c *Client) backoff(failures int) time.Duration {
	wait := c.cfg.LoginBackoffMin
	for i := 1; i < failures && wait < c.cfg.LoginBackoffMax; i++ {
		wait *= 2
	}
	return min(wait, c.cfg.LoginBackoffMax)
}

func (c *Client) login(ctx context.Context) (string, error) {
	form := url.Values{"email": {c.cfg.Email}, "password": {c.cfg.Password}}
	res, err := c.send(ctx, request{
		method:      http.MethodPost,
		path:        loginPath,
		body:        []byte(form.Encode()),
		contentType: "application/x-www-form-urlencoded",
	})
	if err != nil {
		return "", err
	}
	if res.status >= 500 || res.status == http.StatusTooManyRequests {
		return "", fmt.Errorf("%w: login returned HTTP %d", ErrTemporary, res.status)
	}

	var env struct {
		Result struct {
			Token string `json:"token"`
		} `json:"result"`
		Error *envelopeError `json:"error"`
	}
	_ = json.Unmarshal(res.body, &env)
	if res.status != http.StatusOK || env.Error != nil || env.Result.Token == "" {
		code := 0
		if env.Error != nil {
			code = env.Error.Code
		}
		return "", fmt.Errorf("%w: login rejected (HTTP %d, code %d)", ErrUnauthorized, res.status, code)
	}
	c.log.Info("paprika login succeeded")
	return env.Result.Token, nil
}

type envelopeError struct {
	Message string `json:"message"`
	Code    int    `json:"code"`
}

// decodeEnvelope turns a response into its result or a typed error.
func decodeEnvelope(res *response, isWrite bool) (json.RawMessage, error) {
	var env struct {
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	envErr := json.Unmarshal(res.body, &env)
	apiErr := &APIError{Status: res.status}
	hasError := len(env.Error) > 0 && !bytes.Equal(env.Error, []byte("null"))
	if hasError {
		var detail envelopeError
		_ = json.Unmarshal(env.Error, &detail)
		apiErr.Code, apiErr.Message = detail.Code, detail.Message
	}

	switch {
	case res.status >= 300 && res.status < 400:
		apiErr.Message = "unexpected redirect (not followed)"
		return nil, apiErr
	case res.status == http.StatusServiceUnavailable, res.status == http.StatusTooManyRequests:
		return nil, fmt.Errorf("%w: HTTP %d", ErrTemporary, res.status)
	case res.status >= 500:
		if isWrite {
			return nil, fmt.Errorf("%w: HTTP %d: %w", ErrOutcomeUnknown, res.status, apiErr)
		}
		return nil, fmt.Errorf("%w: %w", ErrTemporary, apiErr)
	case res.status < 200 || res.status >= 300:
		return nil, apiErr
	case hasError:
		return nil, apiErr
	case envErr != nil || env.Result == nil:
		return nil, fmt.Errorf("%w: HTTP %d without a result", ErrUnexpectedResponse, res.status)
	}
	return env.Result, nil
}

// send performs one paced HTTP exchange. It does not interpret the status.
func (c *Client) send(ctx context.Context, r request) (*response, error) {
	select {
	case c.gate <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() {
		c.lastEnd = time.Now()
		<-c.gate
	}()

	if wait := c.pausedFor(); wait > 0 {
		return nil, fmt.Errorf("%w: paused after repeated timeouts, resuming in %s", ErrTemporary, wait.Round(time.Second))
	}
	if err := c.pace(ctx); err != nil {
		return nil, err
	}

	rctx, cancel := context.WithTimeout(ctx, c.cfg.RequestTimeout)
	defer cancel()

	var body io.Reader
	if r.body != nil {
		body = bytes.NewReader(r.body)
	}
	req, err := http.NewRequestWithContext(rctx, r.method, c.base+r.path, body)
	if err != nil {
		return nil, fmt.Errorf("%w: build request: %w", ErrInvalidArgument, err)
	}
	req.Header.Set("User-Agent", c.cfg.UserAgent)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Accept-Encoding", "gzip")
	if r.contentType != "" {
		req.Header.Set("Content-Type", r.contentType)
	}
	if r.token != "" {
		req.Header.Set("Authorization", "Bearer "+r.token)
	}

	start := time.Now()
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, c.transportFailure(ctx, r, err)
	}
	defer resp.Body.Close()

	data, err := c.readBody(resp)
	if err != nil {
		if errors.Is(err, ErrResponseTooLarge) {
			c.noteResponse()
			return nil, err
		}
		return nil, c.transportFailure(ctx, r, err)
	}
	c.noteResponse()
	c.log.Debug("paprika request",
		"method", r.method, "path", r.path, "status", resp.StatusCode, "duration", time.Since(start))
	return &response{status: resp.StatusCode, body: data}, nil
}

func (c *Client) pausedFor() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.pausedUntil.Sub(c.cfg.Now())
}

func (c *Client) pace(ctx context.Context) error {
	if c.cfg.MinInterval <= 0 || c.lastEnd.IsZero() {
		return nil
	}
	wait := c.cfg.MinInterval - time.Since(c.lastEnd)
	if wait <= 0 {
		return nil
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *Client) noteResponse() {
	c.mu.Lock()
	c.consecutiveTO = 0
	c.mu.Unlock()
}

// transportFailure classifies an error from sending or reading, and counts
// timeouts toward a pause.
func (c *Client) transportFailure(ctx context.Context, r request, err error) error {
	if ctx.Err() != nil {
		if r.unknownOnFail {
			return fmt.Errorf("%w: %w", ErrOutcomeUnknown, ctx.Err())
		}
		return ctx.Err()
	}

	var netErr net.Error
	timedOut := errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout())
	cause := "request failed"
	if timedOut {
		cause = "request timed out"
		c.mu.Lock()
		c.consecutiveTO++
		if c.consecutiveTO >= c.cfg.TimeoutThreshold {
			c.consecutiveTO = 0
			c.pausedUntil = c.cfg.Now().Add(c.cfg.TimeoutPause)
			c.log.Warn("paprika requests paused after repeated timeouts", "pause", c.cfg.TimeoutPause)
		}
		c.mu.Unlock()
	}
	if r.unknownOnFail {
		return fmt.Errorf("%w: %s: %w", ErrOutcomeUnknown, cause, err)
	}
	return fmt.Errorf("%w: %s: %w", ErrTemporary, cause, err)
}

// readBody reads at most MaxResponseBytes after decompression.
func (c *Client) readBody(resp *http.Response) ([]byte, error) {
	limit := c.cfg.MaxResponseBytes
	if resp.ContentLength > limit {
		return nil, ErrResponseTooLarge
	}
	var reader io.Reader = &cappedReader{r: resp.Body, left: limit}
	if strings.EqualFold(resp.Header.Get("Content-Encoding"), "gzip") {
		zr, err := gzip.NewReader(reader)
		if err != nil {
			return nil, fmt.Errorf("%w: gzip: %w", ErrUnexpectedResponse, err)
		}
		defer zr.Close()
		reader = &cappedReader{r: zr, left: limit}
	}
	data, err := io.ReadAll(reader)
	if err != nil {
		return nil, err
	}
	return data, nil
}

// cappedReader fails with ErrResponseTooLarge once more than left bytes arrive.
type cappedReader struct {
	r    io.Reader
	left int64
}

func (c *cappedReader) Read(p []byte) (int, error) {
	if c.left < 0 {
		return 0, ErrResponseTooLarge
	}
	if int64(len(p)) > c.left+1 {
		p = p[:c.left+1]
	}
	n, err := c.r.Read(p)
	c.left -= int64(n)
	if c.left < 0 {
		return n, ErrResponseTooLarge
	}
	return n, err
}
