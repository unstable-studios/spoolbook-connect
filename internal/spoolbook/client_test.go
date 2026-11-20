package spoolbook

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const (
	testToken   = "sbk_0123456789abcdef_" + "abcdefghijABCDEFGHIJ0123456789_-abcdefghijk"
	testLibrary = "lib_0123456789abcdef"
)

func newTestClient(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()

	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	c, err := New(srv.URL, testToken, testLibrary)
	if err != nil {
		// httptest serves on 127.0.0.1 over http, which ParseBaseURL allows.
		t.Fatal(err)
	}

	return c.WithHTTP(srv.Client())
}

func TestSendWritesTheRequestSpoolbookExpects(t *testing.T) {
	t.Parallel()

	var got struct {
		method, path, auth, ctype string
		body                      map[string]any
	}

	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		got.method, got.path = r.Method, r.URL.Path
		got.auth, got.ctype = r.Header.Get("Authorization"), r.Header.Get("Content-Type")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &got.body)
		_, _ = w.Write([]byte(`{"spool":{},"revision":7,"changed":true}`))
	})

	remaining := 480.5
	tool := 1
	res, err := c.Send(context.Background(), Spool{"prusament", "pla-galaxy-black", "s1"}, Update{
		Remaining: &remaining,
		Print:     &Print{Print: strings.Repeat("a", 64), Tool: &tool, Date: "2026-11-19", Grams: 12.3, File: "benchy.bgcode"},
	})
	if err != nil {
		t.Fatal(err)
	}

	if res.Revision != 7 || !res.Changed {
		t.Errorf("result = %+v", res)
	}

	if got.method != http.MethodPost || got.path != "/api/libraries/"+testLibrary+"/spools/prusament/pla-galaxy-black/s1" {
		t.Errorf("request = %s %s", got.method, got.path)
	}

	if got.auth != "Bearer "+testToken || got.ctype != "application/json" {
		t.Errorf("headers: auth=%q type=%q", got.auth, got.ctype)
	}

	if got.body["remaining"] != 480.5 {
		t.Errorf("remaining = %v", got.body["remaining"])
	}

	p, _ := got.body["print"].(map[string]any)
	if p["grams"] != 12.3 || p["tool"] != 1.0 || p["file"] != "benchy.bgcode" || p["date"] != "2026-11-19" {
		t.Errorf("print = %v", p)
	}

	if _, has := p["again"]; has {
		t.Errorf("again sent when false")
	}
}

func TestSendClassifiesFailures(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		status     int
		header     map[string]string
		body       string
		class      Class
		code       string
		retryAfter time.Duration
	}{
		{"rate limited", 429, map[string]string{"Retry-After": "60"}, `{"error":"rate_limited","message":"Too many"}`, Retry, "rate_limited", time.Minute},
		{"server error", 500, nil, `oops`, Retry, "", 0},
		{"bad gateway", 502, nil, ``, Retry, "", 0},
		{"conflict", 409, nil, `{"error":"conflict"}`, Retry, "conflict", 0},
		{"library invalid", 409, nil, `{"error":"library_invalid"}`, Retry, "library_invalid", 0},
		{"bad token", 401, nil, `{"error":"unauthorized"}`, Permanent, "unauthorized", 0},
		{"forbidden", 403, nil, `{"error":"forbidden"}`, Permanent, "forbidden", 0},
		{"no spool", 404, nil, `{"error":"not_found"}`, Permanent, "not_found", 0},
		{"too large", 413, nil, `{"error":"too_large"}`, Permanent, "too_large", 0},
		{"invalid", 422, nil, `{"error":"invalid"}`, Permanent, "invalid", 0},
		{"bad request", 400, nil, `{"error":"bad_request"}`, Permanent, "bad_request", 0},
		{"redirect", 302, map[string]string{"Location": "https://elsewhere.example/"}, ``, Permanent, "", 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
				for k, v := range tc.header {
					w.Header().Set(k, v)
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			})

			remaining := 1.0
			_, err := c.Send(context.Background(), Spool{"v", "f", "s"}, Update{Remaining: &remaining})

			var failure *Error
			if !errors.As(err, &failure) {
				t.Fatalf("err = %v, want *Error", err)
			}

			if failure.Class != tc.class || failure.Status != tc.status || failure.Code != tc.code || failure.RetryAfter != tc.retryAfter {
				t.Errorf("got class=%v status=%d code=%q retryAfter=%v", failure.Class, failure.Status, failure.Code, failure.RetryAfter)
			}

			if strings.Contains(err.Error(), testToken) {
				t.Errorf("error quotes the token: %v", err)
			}
		})
	}
}

func TestSendDoesNotFollowRedirects(t *testing.T) {
	t.Parallel()

	reached := false
	other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true }))
	t.Cleanup(other.Close)

	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL, http.StatusTemporaryRedirect)
	})

	remaining := 1.0
	if _, err := c.Send(context.Background(), Spool{"v", "f", "s"}, Update{Remaining: &remaining}); err == nil {
		t.Fatal("a redirect succeeded")
	}

	if reached {
		t.Error("the redirect was followed, with the token")
	}
}

func TestSendNetworkErrorRetries(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	addr := srv.URL
	srv.Close()

	c, err := New(addr, testToken, testLibrary)
	if err != nil {
		t.Fatal(err)
	}

	remaining := 1.0
	_, err = c.Send(context.Background(), Spool{"v", "f", "s"}, Update{Remaining: &remaining})

	var failure *Error
	if !errors.As(err, &failure) || failure.Class != Retry || failure.Status != 0 {
		t.Fatalf("err = %#v", err)
	}
}

func TestSendRefusesBadSpoolIDsBeforeSending(t *testing.T) {
	t.Parallel()

	c := newTestClient(t, func(http.ResponseWriter, *http.Request) { t.Error("sent") })

	remaining := 1.0
	_, err := c.Send(context.Background(), Spool{"v", "../x", "s"}, Update{Remaining: &remaining})

	var failure *Error
	if !errors.As(err, &failure) || failure.Class != Permanent {
		t.Fatalf("err = %v", err)
	}
}

func TestNewChecksSettings(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name, url, token, library string
		ok                        bool
	}{
		{"default", "https://spoolbook.unstablestudios.com", testToken, testLibrary, true},
		{"trailing slash", "https://spoolbook.unstablestudios.com/", testToken, testLibrary, true},
		{"localhost http", "http://localhost:8787", testToken, testLibrary, true},
		{"plain http", "http://spoolbook.example", testToken, testLibrary, false},
		{"no host", "https://", testToken, testLibrary, false},
		{"credentials in url", "https://u:p@spoolbook.example", testToken, testLibrary, false},
		{"bad token", "https://spoolbook.example", "sbk_nope", testLibrary, false},
		{"bad library", "https://spoolbook.example", testToken, "short", false},
	}

	for _, tc := range cases {
		_, err := New(tc.url, tc.token, tc.library)
		if (err == nil) != tc.ok {
			t.Errorf("%s: err = %v", tc.name, err)
		}

		if err != nil && strings.Contains(err.Error(), testToken) {
			t.Errorf("%s: error quotes the token", tc.name)
		}
	}
}

func TestParseRetryAfter(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	cases := map[string]time.Duration{
		"":                              0,
		"60":                            time.Minute,
		"junk":                          0,
		"Thu, 19 Nov 2026 12:02:00 GMT": 2 * time.Minute,
		"Thu, 19 Nov 2026 11:00:00 GMT": 0,
	}

	for in, want := range cases {
		if got := parseRetryAfter(in, now); got != want {
			t.Errorf("parseRetryAfter(%q) = %v, want %v", in, got, want)
		}
	}
}
