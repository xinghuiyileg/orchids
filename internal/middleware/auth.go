package middleware

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"sync"
	"time"
)

var (
	sessions   = make(map[string]time.Time)
	sessionMu  sync.RWMutex
	sessionTTL = 24 * time.Hour
)

func init() {
	go cleanupExpiredSessions()
}

func cleanupExpiredSessions() {
	ticker := time.NewTicker(time.Hour)
	for range ticker.C {
		sessionMu.Lock()
		now := time.Now()
		for token, exp := range sessions {
			if now.After(exp) {
				delete(sessions, token)
			}
		}
		sessionMu.Unlock()
	}
}

func generateSessionID() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return ""
	}
	return hex.EncodeToString(b)
}

func validateSession(token string) bool {
	sessionMu.RLock()
	defer sessionMu.RUnlock()
	if exp, ok := sessions[token]; ok {
		return time.Now().Before(exp)
	}
	return false
}

func createSession() string {
	sessionMu.Lock()
	defer sessionMu.Unlock()
	token := generateSessionID()
	sessions[token] = time.Now().Add(sessionTTL)
	return token
}

func checkAuth(r *http.Request, username, password string) bool {
	if cookie, err := r.Cookie("admin_session"); err == nil {
		if validateSession(cookie.Value) {
			return true
		}
	}
	user, pass, ok := r.BasicAuth()
	return ok && user == username && pass == password
}

func setSessionCookie(w http.ResponseWriter) {
	token := createSession()
	http.SetCookie(w, &http.Cookie{
		Name:     "admin_session",
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		MaxAge:   int(sessionTTL.Seconds()),
		SameSite: http.SameSiteLaxMode,
	})
}

func BasicAuth(username, password string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !checkAuth(r, username, password) {
			w.Header().Set("WWW-Authenticate", `Basic realm="Admin"`)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		if _, err := r.Cookie("admin_session"); err != nil {
			setSessionCookie(w)
		}
		next(w, r)
	}
}

func BasicAuthHandler(username, password string, next http.Handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !checkAuth(r, username, password) {
			w.Header().Set("WWW-Authenticate", `Basic realm="Admin"`)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		if _, err := r.Cookie("admin_session"); err != nil {
			setSessionCookie(w)
		}
		next.ServeHTTP(w, r)
	}
}
