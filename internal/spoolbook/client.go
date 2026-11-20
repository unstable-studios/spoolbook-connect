// Package spoolbook is the client for spoolbook's library-token API: one
// write that records a spool's amount left and its prints.
//
// POST /api/libraries/<library>/spools/<vendor>/<filament>/<spool>
// with Authorization: Bearer sbk_<id>_<secret>.
package spoolbook

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// TokenFormat is a library token, as spoolbook issues it.
var TokenFormat = regexp.MustCompile(`^sbk_[0-9a-f]{16}_[A-Za-z0-9_-]{43}$`)

// LibraryFormat is a library id.
var LibraryFormat = regexp.MustCompile(`^[A-Za-z0-9_-]{16,64}$`)

// SlugFormat is a vendor, filament or spool id in a path.
var SlugFormat = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]*$`)

// PrintIDFormat is a print id spoolbook accepts.
var PrintIDFormat = regexp.MustCompile(`^[0-9a-f]{16,64}$`)

// MaxBodyBytes is the largest body spoolbook takes.
const MaxBodyBytes = 8 * 1024

// Spool names one spool in a spoolbook library.
type Spool struct {
	Vendor   string `json:"vendor"`
	Filament string `json:"filament"`
	Spool    string `json:"spool"`
}

// Validate checks each part against what spoolbook's route accepts.
func (s Spool) Validate() error {
	for _, part := range []struct{ name, value string }{
		{"vendor", s.Vendor}, {"filament", s.Filament}, {"spool", s.Spool},
	} {
		if !SlugFormat.MatchString(part.value) || len(part.value) > 100 {
			return fmt.Errorf("%s %q is not a spoolbook id", part.name, part.value)
		}
	}

	return nil
}

func (s Spool) String() string { return s.Vendor + "/" + s.Filament + "/" + s.Spool }

// Print is one print, history on the spool.
type Print struct {
	Print string  `json:"print"`
	Tool  *int    `json:"tool,omitempty"`
	Date  string  `json:"date,omitempty"`
	Grams float64 `json:"grams"`
	File  string  `json:"file,omitempty"`
	Again bool    `json:"again,omitempty"`
}

// Update is the request body. With both fields, Remaining is the amount left
// and Print is history only.
type Update struct {
	Remaining *float64 `json:"remaining,omitempty"`
	Print     *Print   `json:"print,omitempty"`
}

// Result is what a successful write answers.
type Result struct {
	Revision int  `json:"revision"`
	Changed  bool `json:"changed"`
}

// Class says what to do with a failed write.
type Class int

const (
	// Retry means try again later: 429, 5xx, 409 or the network.
	Retry Class = iota
	// Permanent means sending it again won't help: 400, 413, 422.
	Permanent
	// Pause means the setup is wrong until someone fixes it: 401 (token
	// revoked or rotated), 403 (limits, or the token maker's role) or a
	// redirect (spoolbook_url moved). Nothing can go until then.
	Pause
	// Missing is a 404: no such spool, perhaps not made in spoolbook yet.
	Missing
)

// MaxRetryAfter caps what a 429's Retry-After can ask for.
const MaxRetryAfter = time.Hour

// Error is a failed write.
type Error struct {
	Class Class
	// Status is the HTTP status, 0 for a network error.
	Status int
	// Code is spoolbook's error code, such as not_found.
	Code    string
	Message string
	// RetryAfter is what a 429 asked for, 0 when it said nothing.
	RetryAfter time.Duration
	Err        error
}

func (e *Error) Error() string {
	if e.Status == 0 {
		return fmt.Sprintf("spoolbook: %v", e.Err)
	}

	msg := fmt.Sprintf("spoolbook answered %d", e.Status)
	if e.Code != "" {
		msg += " " + e.Code
	}

	if e.Message != "" {
		msg += ": " + e.Message
	}

	return msg
}

func (e *Error) Unwrap() error { return e.Err }

// Classify sorts a failed status.
func Classify(status int) Class {
	switch {
	case status == http.StatusTooManyRequests, status >= 500, status == http.StatusConflict,
		status == http.StatusRequestTimeout:
		return Retry
	case status == http.StatusUnauthorized, status == http.StatusForbidden, status >= 300 && status < 400:
		return Pause
	case status == http.StatusNotFound:
		return Missing
	default:
		return Permanent
	}
}

// Client writes to one library.
type Client struct {
	base    *url.URL
	token   string
	library string
	http    *http.Client
}

// Timeout bounds one request.
const Timeout = 30 * time.Second

// New checks the settings and returns a client. The token is never part of an
// error or a log line.
func New(baseURL, token, library string) (*Client, error) {
	base, err := ParseBaseURL(baseURL)
	if err != nil {
		return nil, err
	}

	if !TokenFormat.MatchString(token) {
		return nil, errors.New("token is not a spoolbook library token (sbk_…)")
	}

	if !LibraryFormat.MatchString(library) {
		return nil, errors.New("library is not a spoolbook library id")
	}

	return &Client{base: base, token: token, library: library, http: NewHTTPClient()}, nil
}

// ParseBaseURL accepts https, or http for localhost only.
func ParseBaseURL(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimRight(raw, "/"))
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("spoolbook_url %q is not an address", raw)
	}

	local := u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1" || u.Hostname() == "::1"
	if u.Scheme != "https" && !(u.Scheme == "http" && local) {
		return nil, fmt.Errorf("spoolbook_url %q must be https", raw)
	}

	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("spoolbook_url %q must be a plain address", raw)
	}

	return u, nil
}

// NewHTTPClient has a timeout and follows no redirect: a redirect would carry
// the token somewhere the operator didn't name.
func NewHTTPClient() *http.Client {
	return &http.Client{
		Timeout: Timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// WithHTTP swaps the HTTP client, for tests. Redirects stay off.
func (c *Client) WithHTTP(h *http.Client) *Client {
	h.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	c.http = h

	return c
}

// Host is the host the client talks to.
func (c *Client) Host() string { return c.base.Host }

// Send makes one write. A failure is always an *Error.
func (c *Client) Send(ctx context.Context, spool Spool, update Update) (Result, error) {
	if err := spool.Validate(); err != nil {
		return Result{}, &Error{Class: Permanent, Err: err}
	}

	body, err := json.Marshal(update)
	if err != nil {
		return Result{}, &Error{Class: Permanent, Err: err}
	}

	if len(body) > MaxBodyBytes {
		return Result{}, &Error{Class: Permanent, Status: http.StatusRequestEntityTooLarge, Err: errors.New("body over 8 KB")}
	}

	target := c.base.JoinPath("api", "libraries", c.library, "spools", spool.Vendor, spool.Filament, spool.Spool)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target.String(), bytes.NewReader(body))
	if err != nil {
		return Result{}, &Error{Class: Permanent, Err: err}
	}

	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "spoolbook-connect")

	res, err := c.http.Do(req)
	if err != nil {
		return Result{}, &Error{Class: Retry, Err: redact(err, c.token)}
	}
	defer res.Body.Close()

	raw, _ := io.ReadAll(io.LimitReader(res.Body, 64<<10))

	if res.StatusCode == http.StatusOK {
		var out Result
		if err := json.Unmarshal(raw, &out); err != nil {
			return Result{}, &Error{Class: Retry, Status: res.StatusCode, Err: fmt.Errorf("unreadable answer: %w", err)}
		}

		return out, nil
	}

	failure := &Error{Class: Classify(res.StatusCode), Status: res.StatusCode}
	if res.StatusCode >= 300 && res.StatusCode < 400 {
		failure.Message = "redirected to " + res.Header.Get("Location") + "; set spoolbook_url to the final address"
	}

	var answer struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &answer) == nil {
		failure.Code, failure.Message = answer.Error, firstNonEmpty(answer.Message, failure.Message)
	}

	if res.StatusCode == http.StatusTooManyRequests {
		failure.RetryAfter = parseRetryAfter(res.Header.Get("Retry-After"), time.Now())
	}

	return Result{}, failure
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}

	return b
}

// parseRetryAfter reads seconds or an HTTP date.
func parseRetryAfter(v string, now time.Time) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}

	var d time.Duration
	if secs, err := strconv.Atoi(v); err == nil && secs >= 0 {
		d = time.Duration(min(secs, int(MaxRetryAfter/time.Second))) * time.Second
	} else if at, err := http.ParseTime(v); err == nil && at.After(now) {
		d = at.Sub(now)
	}

	return min(d, MaxRetryAfter)
}

// redact keeps the token out of an error, should a transport ever quote it.
func redact(err error, token string) error {
	if token == "" || !strings.Contains(err.Error(), token) {
		return err
	}

	return errors.New(strings.ReplaceAll(err.Error(), token, "sbk_…"))
}
