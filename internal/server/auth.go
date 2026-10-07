package server

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/worotyns/agg/internal/store"
)

const (
	sessionCookie     = "agg_session"
	sessionTTL        = 30 * 24 * time.Hour
	settingAdminHash  = "admin_token_hash"
	settingSessionKey = "session_secret"

	// authFailuresPerMinute caps wrong admin/API token attempts per client IP (UI login, Bearer, MCP).
	// Successful attempts are not counted.
	authFailuresPerMinute = 5
)

// EnsureAdminToken makes sure an admin token exists. If envToken is set it becomes the token.
// Otherwise a token is generated on first start and returned so it can be printed once.
func EnsureAdminToken(ctx context.Context, st store.Storage, envToken string) (generated string, err error) {
	if envToken != "" {
		return "", st.SetSetting(ctx, settingAdminHash, hashToken(envToken))
	}
	_, err = st.GetSetting(ctx, settingAdminHash)
	if err == nil {
		return "", nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return "", err
	}
	return ResetAdminToken(ctx, st)
}

// ResetAdminToken generates and stores a new admin token, invalidating existing sessions.
func ResetAdminToken(ctx context.Context, st store.Storage) (string, error) {
	t := "agg_admin_" + randomString(32)
	return t, st.SetSetting(ctx, settingAdminHash, hashToken(t))
}

func (s *Server) sessionSecret(ctx context.Context) []byte {
	s.secretOnce.Do(func() {
		v, err := s.st.GetSetting(ctx, settingSessionKey)
		if err != nil {
			b := make([]byte, 32)
			rand.Read(b)
			v = hex.EncodeToString(b)
			if err := s.st.SetSetting(ctx, settingSessionKey, v); err != nil {
				s.log.Error("cannot store session secret", "err", err)
			}
		}
		s.secret = []byte(v)
	})
	return s.secret
}

func (s *Server) adminHash(ctx context.Context) string {
	h, _ := s.st.GetSetting(ctx, settingAdminHash)
	return h
}

// sign binds a session to its expiry and to the current admin token, so changing the token logs everyone out.
func (s *Server) sign(ctx context.Context, exp int64) string {
	m := hmac.New(sha256.New, s.sessionSecret(ctx))
	m.Write([]byte(strconv.FormatInt(exp, 10) + "|" + s.adminHash(ctx)))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

func (s *Server) validSession(r *http.Request) bool {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return false
	}
	exp, sig, ok := strings.Cut(c.Value, ".")
	if !ok {
		return false
	}
	e, err := strconv.ParseInt(exp, 10, 64)
	if err != nil || time.Now().Unix() > e {
		return false
	}
	return hmac.Equal([]byte(sig), []byte(s.sign(r.Context(), e)))
}

// validToken accepts the admin token or a named API token.
func (s *Server) validToken(ctx context.Context, token string) bool {
	if token == "" {
		return false
	}
	h := s.adminHash(ctx)
	if h != "" && subtle.ConstantTimeCompare([]byte(hashToken(token)), []byte(h)) == 1 {
		return true
	}
	return s.apiTokenValid(ctx, token)
}

func bearer(r *http.Request) string {
	a := r.Header.Get("Authorization")
	if len(a) > 7 && strings.EqualFold(a[:7], "bearer ") {
		return strings.TrimSpace(a[7:])
	}
	return ""
}

func (s *Server) requireAdmin(h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if t := bearer(r); t != "" {
			if !s.checkToken(w, r, func() bool { return s.validToken(r.Context(), t) }) {
				return
			}
		} else {
			if !s.validSession(r) {
				writeErr(w, 401, "login required")
				return
			}
			// Cookie-authenticated writes must come from our own UI (JSON body, same origin).
			if r.Method != http.MethodGet && r.Method != http.MethodHead {
				if o := r.Header.Get("Origin"); o != "" && o != s.baseURL(r) {
					writeErr(w, 403, "cross-origin request rejected")
					return
				}
			}
		}
		w.Header().Set("Cache-Control", "no-store")
		h(w, r)
	})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Token string `json:"token"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	if !s.checkToken(w, r, func() bool { return s.validAdminToken(r.Context(), strings.TrimSpace(body.Token)) }) {
		return
	}
	exp := time.Now().Add(sessionTTL).Unix()
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: strconv.FormatInt(exp, 10) + "." + s.sign(r.Context(), exp),
		Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: strings.HasPrefix(s.baseURL(r), "https"),
		Expires: time.Unix(exp, 0),
	})
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// validAdminToken accepts only the admin token (UI login); API tokens are for programs.
func (s *Server) validAdminToken(ctx context.Context, token string) bool {
	h := s.adminHash(ctx)
	return h != "" && token != "" && subtle.ConstantTimeCompare([]byte(hashToken(token)), []byte(h)) == 1
}

// checkToken runs valid under the per-IP failed-attempt limit, writing 429 or 401 when it does not pass.
// A blocked client is rejected before its token is checked, so guessing is capped even with a correct guess.
func (s *Server) checkToken(w http.ResponseWriter, r *http.Request, valid func() bool) bool {
	ip := s.clientIP(r)
	if s.authLimit.Blocked(ip) {
		w.Header().Set("Retry-After", "60")
		writeErr(w, 429, "too many failed attempts, try again in a minute")
		return false
	}
	if valid() {
		return true
	}
	s.authLimit.Allow(ip)
	writeErr(w, 401, "invalid token")
	return false
}
