// Package server implements the needle-server HTTP surface: an ingest
// endpoint for agents, a read API for the dashboard, and the dashboard's
// own static assets.
package server

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	sessionCookie = "needle_session"

	// sessionTTL bounds how long a dashboard login lasts before the user has
	// to type the password again.
	sessionTTL = 30 * 24 * time.Hour
)

// auth holds the two independent credentials: the agent token guarding
// ingest, and the dashboard password guarding reads. Sessions are stateless
// signed cookies, so nothing about them needs to be stored.
type auth struct {
	agentToken string
	password   string
	secret     []byte
	secure     bool
	enabled    bool
}

func newAuth(agentToken, password, secret string, secure bool) *auth {
	if secret == "" {
		// Deriving the signing key from both credentials means changing
		// either one invalidates outstanding sessions, which is the
		// behaviour you want from a "log everyone out" knob.
		sum := sha256.Sum256([]byte("needle\x00" + agentToken + "\x00" + password))
		secret = hex.EncodeToString(sum[:])
	}
	return &auth{
		agentToken: agentToken,
		password:   password,
		secret:     []byte(secret),
		secure:     secure,
		enabled:    password != "",
	}
}

// checkAgent validates the Bearer token on POST /report.
func (a *auth) checkAgent(r *http.Request) bool {
	if a.agentToken == "" {
		return false
	}
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return false
	}
	return hmac.Equal([]byte(strings.TrimSpace(tok)), []byte(a.agentToken))
}

// checkSession validates the dashboard session cookie.
func (a *auth) checkSession(r *http.Request) bool {
	if !a.enabled {
		return true
	}
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return false
	}
	payload, sig, ok := strings.Cut(c.Value, ".")
	if !ok {
		return false
	}
	if !hmac.Equal([]byte(sig), []byte(a.sign(payload))) {
		return false
	}
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return false
	}
	exp, err := strconv.ParseInt(string(raw), 10, 64)
	if err != nil {
		return false
	}
	return time.Now().Before(time.Unix(exp, 0))
}

// login checks the password and, on success, sets a session cookie.
func (a *auth) login(w http.ResponseWriter, r *http.Request, password string) bool {
	if !a.enabled {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "auth": false})
		return true
	}
	// The delay blunts online guessing without giving an attacker a way to
	// lock the real user out.
	time.Sleep(200 * time.Millisecond)
	if !hmac.Equal([]byte(password), []byte(a.password)) {
		slog.Warn("dashboard login failed", "remote", r.RemoteAddr)
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "invalid password"})
		return false
	}
	expires := time.Now().Add(sessionTTL)
	payload := encodePayload(expires.Unix())
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    payload + "." + a.sign(payload),
		Path:     "/",
		Expires:  expires,
		MaxAge:   int(sessionTTL.Seconds()),
		HttpOnly: true,
		Secure:   a.secure,
		SameSite: http.SameSiteLaxMode,
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "auth": true})
	return true
}

func (a *auth) logout(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   a.secure,
		SameSite: http.SameSiteLaxMode,
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// encodePayload is the session's public half: the expiry, base64url encoded.
func encodePayload(expiry int64) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(expiry, 10)))
}

func (a *auth) sign(payload string) string {
	mac := hmac.New(sha256.New, a.secret)
	mac.Write([]byte("session\x00"))
	mac.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
