package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSessionRoundTrip(t *testing.T) {
	a := newAuth("agent-token", "hunter2", "", false)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/login", strings.NewReader(`{"password":"hunter2"}`))
	if !a.login(rec, req, "hunter2") {
		t.Fatal("login with the right password was rejected")
	}
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("expected one cookie, got %d", len(cookies))
	}

	// The same cookie must keep validating: this is the path a dashboard
	// hits on every refresh.
	for i := 0; i < 5; i++ {
		next := httptest.NewRequest(http.MethodGet, "/api/nodes", nil)
		next.AddCookie(cookies[0])
		if !a.checkSession(next) {
			t.Fatalf("session rejected on request %d", i+1)
		}
	}
}

func TestSessionRejectsBadCredentials(t *testing.T) {
	a := newAuth("agent-token", "hunter2", "", false)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/login", nil)
	if a.login(rec, req, "wrong") {
		t.Fatal("login with the wrong password was accepted")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}

func TestSessionRejectsTampering(t *testing.T) {
	a := newAuth("agent-token", "hunter2", "", false)

	cases := map[string]string{
		"empty":        "",
		"no signature": "MTc5NDU0MzEyNg",
		"garbage":      "not.a.cookie",
		"foreign sign": "MTc5NDU0MzEyNg.AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		"expired":      expiredCookie(t, a),
	}
	for name, value := range cases {
		req := httptest.NewRequest(http.MethodGet, "/api/nodes", nil)
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: value})
		if a.checkSession(req) {
			t.Errorf("%s: forged session was accepted", name)
		}
	}
}

func TestSessionBoundToCredentials(t *testing.T) {
	a := newAuth("agent-token", "hunter2", "", false)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/login", nil)
	a.login(rec, req, "hunter2")
	cookie := rec.Result().Cookies()[0]

	// Rotating either secret must invalidate outstanding sessions.
	rotated := newAuth("agent-token", "new-password", "", false)
	next := httptest.NewRequest(http.MethodGet, "/api/nodes", nil)
	next.AddCookie(cookie)
	if rotated.checkSession(next) {
		t.Fatal("session survived a password change")
	}
}

func TestAgentToken(t *testing.T) {
	a := newAuth("s3cret", "hunter2", "", false)

	ok := httptest.NewRequest(http.MethodPost, "/report", nil)
	ok.Header.Set("Authorization", "Bearer s3cret")
	if !a.checkAgent(ok) {
		t.Fatal("valid agent token rejected")
	}

	for name, header := range map[string]string{
		"missing": "",
		"bare":    "s3cret",
		"wrong":   "Bearer nope",
		"prefix":  "Bearer s3cre",
		"basic":   "Basic s3cret",
	} {
		bad := httptest.NewRequest(http.MethodPost, "/report", nil)
		if header != "" {
			bad.Header.Set("Authorization", header)
		}
		if a.checkAgent(bad) {
			t.Errorf("%s: bad agent token accepted", name)
		}
	}
}

func TestAuthDisabled(t *testing.T) {
	a := newAuth("s3cret", "", "", false)
	req := httptest.NewRequest(http.MethodGet, "/api/nodes", nil)
	if !a.checkSession(req) {
		t.Fatal("session check should pass when no password is configured")
	}
}

func expiredCookie(t *testing.T, a *auth) string {
	t.Helper()
	past := time.Now().Add(-time.Hour)
	payload := encodePayload(past.Unix())
	return payload + "." + a.sign(payload)
}
