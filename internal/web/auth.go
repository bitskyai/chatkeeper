package web

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"github.com/bitskyai/chatkeeper/internal/config"
	"golang.org/x/crypto/argon2"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func randomToken() string {
	b := make([]byte, 32)
	if _, e := rand.Read(b); e != nil {
		panic("random source unavailable")
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
func equal(a, b string) bool {
	x := sha256.Sum256([]byte(a))
	y := sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(x[:], y[:]) == 1
}
func passwordHash(password string) string {
	salt := make([]byte, 16)
	_, _ = rand.Read(salt)
	hash := argon2.IDKey([]byte(password), salt, 2, 64*1024, 2, 32)
	return "$argon2id$v=19$m=65536,t=2,p=2$" + base64.RawStdEncoding.EncodeToString(salt) + "$" + base64.RawStdEncoding.EncodeToString(hash)
}
func passwordMatches(hash, password string) bool {
	parts := strings.Split(hash, "$")
	if len(parts) != 6 || parts[1] != "argon2id" || parts[2] != "v=19" || parts[3] != "m=65536,t=2,p=2" {
		return false
	}
	salt, e := base64.RawStdEncoding.DecodeString(parts[4])
	if e != nil || len(salt) != 16 {
		return false
	}
	want, e := base64.RawStdEncoding.DecodeString(parts[5])
	if e != nil || len(want) != 32 {
		return false
	}
	got := argon2.IDKey([]byte(password), salt, 2, 64*1024, 2, 32)
	return subtle.ConstantTimeCompare(got, want) == 1
}
func (a *App) adminExists() bool {
	_, e := os.Stat(filepath.Join(a.opts.Home, "state", "admin.hash"))
	return !os.IsNotExist(e)
}
func (a *App) savePassword(password string) error {
	if len(password) < 12 || len(password) > 1024 {
		return errors.New("use a password of 12 to 1024 characters")
	}
	return config.AtomicWrite(filepath.Join(a.opts.Home, "state", "admin.hash"), []byte(passwordHash(password)), 0600)
}
func (a *App) newSession(w http.ResponseWriter, r *http.Request) error {
	id, csrf := randomToken(), randomToken()
	sum := sha256.Sum256([]byte(id))
	_, e := a.db.Exec("INSERT INTO sessions(id,csrf,expires) VALUES(?,?,?)", fmt.Sprintf("%x", sum), csrf, time.Now().Add(12*time.Hour).Unix())
	if e == nil {
		http.SetCookie(w, &http.Cookie{Name: a.cookieName(r), Value: id, Path: "/", HttpOnly: true, Secure: r.TLS != nil || a.opts.SecureCookies, SameSite: http.SameSiteStrictMode, MaxAge: 43200})
	}
	return e
}
func (a *App) session(r *http.Request) (string, string) {
	c, e := r.Cookie(a.cookieName(r))
	if e != nil {
		return "", ""
	}
	sum := sha256.Sum256([]byte(c.Value))
	id := fmt.Sprintf("%x", sum)
	var csrf string
	if a.db.QueryRow("SELECT csrf FROM sessions WHERE id=? AND expires>?", id, time.Now().Unix()).Scan(&csrf) != nil {
		return "", ""
	}
	return id, csrf
}
func (a *App) allowedHost(r *http.Request) bool {
	host := r.Host
	if h, _, e := net.SplitHostPort(host); e == nil {
		host = h
	}
	for _, h := range a.opts.AllowedHosts {
		if strings.EqualFold(host, h) {
			return true
		}
	}
	return false
}
func (a *App) bootstrap(w http.ResponseWriter, r *http.Request) {
	if a.adminExists() {
		http.Error(w, "Administrator already configured", http.StatusConflict)
		return
	}
	if a.existingConfig {
		http.Error(w, "Set CHATKEEPER_ADMIN_PASSWORD on the server to claim this existing installation", http.StatusForbidden)
		return
	}
	if a.opts.Mode != "server" {
		http.NotFound(w, r)
		return
	}
	if r.Method == "GET" {
		a.render(w, page{Title: "Welcome to ChatKeeper", Kind: "bootstrap", Body: "Enter the one-time setup code printed by the server, then choose an administrator password."})
		return
	}
	if r.Method != "POST" {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	a.authMu.Lock()
	defer a.authMu.Unlock()
	if a.adminExists() {
		http.Error(w, "Administrator already configured", http.StatusConflict)
		return
	}
	if !a.allowAttempt(r) {
		http.Error(w, "Too many attempts. Try again in a minute.", http.StatusTooManyRequests)
		return
	}
	if a.opts.SetupCode == "" || !equal(r.FormValue("code"), a.opts.SetupCode) {
		http.Error(w, "Invalid setup code", http.StatusForbidden)
		return
	}
	if e := a.savePassword(r.FormValue("password")); e != nil {
		http.Error(w, e.Error(), http.StatusBadRequest)
		return
	}
	a.opts.SetupCode = ""
	if a.newSession(w, r) != nil {
		http.Error(w, "Session unavailable", http.StatusServiceUnavailable)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

type attempts struct {
	count int
	since time.Time
}

func (a *App) allowAttempt(r *http.Request) bool {
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	now := time.Now()
	v := a.attempts[host]
	if now.Sub(v.since) > time.Minute {
		v = attempts{since: now}
	}
	v.count++
	if len(a.attempts) > 1000 {
		for k, v := range a.attempts {
			if now.Sub(v.since) > time.Minute {
				delete(a.attempts, k)
			}
		}
	}
	a.attempts[host] = v
	return v.count <= 5
}
func (a *App) login(w http.ResponseWriter, r *http.Request) {
	if a.opts.Mode == "desktop" {
		if r.Method == "GET" && a.launchToken != "" && equal(r.URL.Query().Get("token"), a.launchToken) {

			if a.newSession(w, r) != nil {
				http.Error(w, "Session unavailable", http.StatusServiceUnavailable)
				return
			}
			http.Redirect(w, r, safeNext(r.URL.Query().Get("next")), http.StatusSeeOther)
			return
		}
		http.Error(w, "Open the launch URL from this device", http.StatusUnauthorized)
		return
	}
	if r.Method == "GET" {
		a.render(w, page{Title: "Sign in", Kind: "login"})
		return
	}
	if r.Method != "POST" {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	a.authMu.Lock()
	defer a.authMu.Unlock()
	if !a.allowAttempt(r) {
		http.Error(w, "Too many attempts. Try again in a minute.", http.StatusTooManyRequests)
		return
	}
	hash, e := os.ReadFile(filepath.Join(a.opts.Home, "state", "admin.hash"))
	if e != nil || !passwordMatches(string(hash), r.FormValue("password")) {
		http.Error(w, "Invalid password", http.StatusUnauthorized)
		return
	}
	if a.newSession(w, r) != nil {
		http.Error(w, "Session unavailable", http.StatusServiceUnavailable)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// Desktop cookie names include the listener port because browsers share cookies
// across ports on the same host. The server cookie remains stable behind proxies.
func (a *App) cookieName(r *http.Request) string {
	if a.opts.Mode == "desktop" {
		_, port, err := net.SplitHostPort(r.Host)
		if err == nil {
			return "ck_session_" + port
		}
		if r.TLS != nil {
			return "ck_session_443"
		}
		return "ck_session_80"
	}
	return "ck_session"
}
func (a *App) refreshCookie(w http.ResponseWriter, r *http.Request) {
	if c, e := r.Cookie(a.cookieName(r)); e == nil {
		sum := sha256.Sum256([]byte(c.Value))
		var expires int64
		if a.db.QueryRow("SELECT expires FROM sessions WHERE id=?", fmt.Sprintf("%x", sum)).Scan(&expires) != nil {
			return
		}
		remaining := int(expires - time.Now().Unix())
		if remaining <= 0 {
			return
		}
		http.SetCookie(w, &http.Cookie{Name: a.cookieName(r), Value: c.Value, Path: "/", HttpOnly: true, Secure: r.TLS != nil || a.opts.SecureCookies, SameSite: http.SameSiteStrictMode, MaxAge: remaining})
	}
}

func validPasswordHash(hash string) bool {
	parts := strings.Split(hash, "$")
	if len(parts) != 6 || parts[1] != "argon2id" || parts[2] != "v=19" || parts[3] != "m=65536,t=2,p=2" {
		return false
	}
	salt, e := base64.RawStdEncoding.DecodeString(parts[4])
	if e != nil || len(salt) != 16 {
		return false
	}
	hashBytes, e := base64.RawStdEncoding.DecodeString(parts[5])
	return e == nil && len(hashBytes) == 32
}
