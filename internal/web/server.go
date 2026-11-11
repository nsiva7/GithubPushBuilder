// Package web is the dashboard: login, status, live logs, artifacts and admin.
package web

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"fmt"
	"html/template"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/nsiva7/GithubPushBuilder/internal/config"
	"github.com/nsiva7/GithubPushBuilder/internal/engine"
	"github.com/nsiva7/GithubPushBuilder/internal/store"
)

//go:embed templates/*.html
var templateFS embed.FS

const (
	sessionCookie = "gpb_session"
	flashCookie   = "gpb_flash"
	sessionTTL    = 7 * 24 * time.Hour
	minPassword   = 10
)

type Server struct {
	cfg     config.Config
	st      *store.Store
	eng     *engine.Engine
	hook    http.Handler
	mux     *http.ServeMux
	pages   map[string]*template.Template
	limiter *loginLimiter
}

// reqInfo is what the auth layer learned about a request.
type reqInfo struct {
	LoggedIn  bool
	CSRF      string
	Nonce     string
	TokenHash string
}

type ctxKey struct{}

func info(r *http.Request) *reqInfo { return r.Context().Value(ctxKey{}).(*reqInfo) }

func New(cfg config.Config, st *store.Store, eng *engine.Engine, hook http.Handler) (*Server, error) {
	s := &Server{cfg: cfg, st: st, eng: eng, hook: hook, mux: http.NewServeMux(),
		pages: map[string]*template.Template{}, limiter: newLoginLimiter()}
	for _, name := range []string{"login", "index", "build", "repos", "repo", "secrets", "settings"} {
		t, err := template.New(name).Funcs(funcs).ParseFS(templateFS, "templates/layout.html", "templates/"+name+".html")
		if err != nil {
			return nil, err
		}
		s.pages[name] = t
	}

	m := s.mux
	m.HandleFunc("GET /{$}", s.index)
	m.HandleFunc("POST /logout", s.logout)
	m.HandleFunc("POST /run", s.runManual)
	m.HandleFunc("GET /builds/{id}", s.buildPage)
	m.HandleFunc("GET /builds/{id}/events", s.buildEvents)
	m.HandleFunc("GET /builds/{id}/log", s.buildLog)
	m.HandleFunc("GET /builds/{id}/artifacts/{path...}", s.artifact)
	m.HandleFunc("POST /builds/{id}/cancel", s.buildCancel)
	m.HandleFunc("POST /builds/{id}/retry", s.buildRetry)
	m.HandleFunc("GET /repos", s.reposPage)
	m.HandleFunc("POST /repos", s.repoCreate)
	m.HandleFunc("GET /repos/{id}", s.repoPage)
	m.HandleFunc("POST /repos/{id}", s.repoUpdate)
	m.HandleFunc("POST /repos/{id}/secret", s.repoNewSecret)
	m.HandleFunc("POST /repos/{id}/delete", s.repoDelete)
	m.HandleFunc("GET /secrets", s.secretsPage)
	m.HandleFunc("POST /secrets", s.secretCreate)
	m.HandleFunc("POST /secrets/{id}/delete", s.secretDelete)
	m.HandleFunc("GET /settings", s.settingsPage)
	m.HandleFunc("POST /settings", s.settingsSave)
	m.HandleFunc("POST /settings/password", s.passwordChange)
	return s, nil
}

// ServeHTTP is the single gate in front of everything. Only the webhook and
// the login form are reachable without a session.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/hooks/github" {
		s.hook.ServeHTTP(w, r)
		return
	}

	in := &reqInfo{Nonce: randomHex(16)}
	h := w.Header()
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'nonce-"+in.Nonce+"'; script-src 'nonce-"+in.Nonce+
		"'; connect-src 'self'; img-src 'self' data:; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "same-origin")
	h.Set("Cache-Control", "no-store")

	if c, err := r.Cookie(sessionCookie); err == nil {
		hash := hashToken(c.Value)
		if csrf, ok := s.st.Session(hash); ok {
			in.LoggedIn, in.CSRF, in.TokenHash = true, csrf, hash
		}
	}
	r = r.WithContext(context.WithValue(r.Context(), ctxKey{}, in))
	if r.Method == http.MethodPost {
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	}

	if r.URL.Path == "/login" {
		switch r.Method {
		case http.MethodGet, http.MethodHead:
			s.loginPage(w, r)
		case http.MethodPost:
			s.loginSubmit(w, r)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
		return
	}
	if !in.LoggedIn {
		if r.Method == http.MethodGet && strings.Contains(r.Header.Get("Accept"), "text/html") {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
		} else {
			http.Error(w, "login required", http.StatusUnauthorized)
		}
		return
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead:
	case http.MethodPost:
		if subtle.ConstantTimeCompare([]byte(r.PostFormValue("csrf")), []byte(in.CSRF)) != 1 {
			http.Error(w, "This form has expired. Go back, reload the page and try again.", http.StatusForbidden)
			return
		}
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.mux.ServeHTTP(w, r)
}

// ---- rendering ----

type pageData struct {
	Service   string
	Title     string
	Nav       string
	LoggedIn  bool
	CSRF      string
	Nonce     string
	Flash     string
	FlashKind string // ok or err
	D         any
}

func (s *Server) page(w http.ResponseWriter, r *http.Request, title, nav string, d any) pageData {
	in := info(r)
	p := pageData{Service: config.ServiceName, Title: title, Nav: nav,
		LoggedIn: in.LoggedIn, CSRF: in.CSRF, Nonce: in.Nonce, D: d}
	if c, err := r.Cookie(flashCookie); err == nil {
		if kind, msg, ok := strings.Cut(c.Value, ":"); ok {
			if msg, err := url.QueryUnescape(msg); err == nil {
				p.Flash, p.FlashKind = msg, kind
			}
		}
		http.SetCookie(w, &http.Cookie{Name: flashCookie, Path: "/", MaxAge: -1})
	}
	return p
}

func (s *Server) render(w http.ResponseWriter, r *http.Request, name, title, nav string, d any) {
	s.exec(w, name, "layout", s.page(w, r, title, nav, d))
}

func (s *Server) exec(w http.ResponseWriter, name, tpl string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.pages[name].ExecuteTemplate(w, tpl, data); err != nil {
		log.Printf("render %s: %v", name, err)
	}
}

// flash stores a one-time message shown on the next page.
func (s *Server) flash(w http.ResponseWriter, kind, format string, args ...any) {
	http.SetCookie(w, &http.Cookie{
		Name: flashCookie, Value: kind + ":" + url.QueryEscape(fmt.Sprintf(format, args...)),
		Path: "/", MaxAge: 60, HttpOnly: true, Secure: !s.cfg.InsecureCookie, SameSite: http.SameSiteLaxMode,
	})
}

func (s *Server) back(w http.ResponseWriter, r *http.Request, to string) {
	http.Redirect(w, r, to, http.StatusSeeOther)
}

func (s *Server) fail(w http.ResponseWriter, err error) {
	log.Printf("web: %v", err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}

func pathID(r *http.Request) int64 {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	return id
}

// ---- login ----

func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	if info(r).LoggedIn {
		s.back(w, r, "/")
		return
	}
	s.render(w, r, "login", "Sign in", "", nil)
}

func (s *Server) loginSubmit(w http.ResponseWriter, r *http.Request) {
	ip := s.clientIP(r)
	if !s.limiter.allow(ip) {
		w.Header().Set("Retry-After", "900")
		w.WriteHeader(http.StatusTooManyRequests)
		s.exec(w, "login", "layout", pageData{Service: config.ServiceName, Title: "Sign in", Nonce: info(r).Nonce,
			Flash: "Too many failed attempts. Try again in 15 minutes.", FlashKind: "err"})
		return
	}
	if !checkPassword(s.st, r.PostFormValue("password")) {
		s.limiter.fail(ip)
		log.Printf("login: failed attempt from %s", ip)
		w.WriteHeader(http.StatusUnauthorized)
		s.exec(w, "login", "layout", pageData{Service: config.ServiceName, Title: "Sign in", Nonce: info(r).Nonce,
			Flash: "Wrong password.", FlashKind: "err"})
		return
	}
	s.limiter.reset(ip)
	token := randomHex(32)
	if err := s.st.CreateSession(hashToken(token), randomHex(32), sessionTTL); err != nil {
		s.fail(w, err)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: token, Path: "/", MaxAge: int(sessionTTL.Seconds()),
		HttpOnly: true, Secure: !s.cfg.InsecureCookie, SameSite: http.SameSiteLaxMode,
	})
	s.back(w, r, "/")
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	s.st.DeleteSession(info(r).TokenHash)
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Path: "/", MaxAge: -1})
	s.back(w, r, "/login")
}

// clientIP is the address used for login rate limiting.
func (s *Server) clientIP(r *http.Request) string {
	if s.cfg.TrustProxy {
		// The right-most entry is the one our own reverse proxy added.
		parts := strings.Split(r.Header.Get("X-Forwarded-For"), ",")
		if ip := strings.TrimSpace(parts[len(parts)-1]); ip != "" {
			return ip
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func checkPassword(st *store.Store, password string) bool {
	hash := st.Setting(store.KeyAdminPassHash)
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
}

// SetPassword stores a new admin password hash.
func SetPassword(st *store.Store, password string) error {
	if len(password) < minPassword {
		return fmt.Errorf("the password needs at least %d characters", minPassword)
	}
	if len(password) > 72 {
		return fmt.Errorf("the password can be at most 72 characters")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), 12)
	if err != nil {
		return err
	}
	return st.SetSetting(store.KeyAdminPassHash, string(hash))
}

// EnsureAdminPassword sets the admin password on first start: from the
// configured value, or a generated one, which it returns so it can be shown once.
func EnsureAdminPassword(st *store.Store, initial string) (generated string, err error) {
	if st.Setting(store.KeyAdminPassHash) != "" {
		return "", nil
	}
	if initial != "" {
		return "", SetPassword(st, initial)
	}
	generated = randomHex(12)
	return generated, SetPassword(st, generated)
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// loginLimiter allows a handful of failed logins per address in a time window.
type loginLimiter struct {
	mu    sync.Mutex
	fails map[string]*failures
}

type failures struct {
	count int
	since time.Time
}

const (
	loginMaxFails = 5
	loginWindow   = 15 * time.Minute
)

func newLoginLimiter() *loginLimiter { return &loginLimiter{fails: map[string]*failures{}} }

func (l *loginLimiter) allow(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	f := l.fails[ip]
	if f == nil {
		return true
	}
	if time.Since(f.since) > loginWindow {
		delete(l.fails, ip)
		return true
	}
	return f.count < loginMaxFails
}

func (l *loginLimiter) fail(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for k, f := range l.fails { // keep the map from growing without bound
		if time.Since(f.since) > loginWindow {
			delete(l.fails, k)
		}
	}
	f := l.fails[ip]
	if f == nil {
		f = &failures{since: time.Now()}
		l.fails[ip] = f
	}
	f.count++
}

func (l *loginLimiter) reset(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.fails, ip)
}

// ---- template helpers ----

var funcs = template.FuncMap{
	"short": func(sha string) string {
		if len(sha) > 7 {
			return sha[:7]
		}
		return sha
	},
	"label": func(status string) string { return strings.ReplaceAll(status, "_", " ") },
	"ts": func(unix int64) template.HTML {
		if unix == 0 {
			return "–"
		}
		t := time.Unix(unix, 0).UTC()
		return template.HTML(fmt.Sprintf(`<time datetime="%s" data-ts="%d">%s UTC</time>`,
			t.Format(time.RFC3339), unix, t.Format("2006-01-02 15:04")))
	},
	"dur": func(b store.Build) string {
		if b.StartedAt == 0 {
			return "–"
		}
		end := b.FinishedAt
		if end == 0 {
			end = time.Now().Unix()
		}
		return fmtDuration(time.Duration(end-b.StartedAt) * time.Second)
	},
	"bytes": func(n int64) string {
		const unit = 1024
		if n < unit {
			return fmt.Sprintf("%d B", n)
		}
		div, exp := int64(unit), 0
		for m := n / unit; m >= unit; m /= unit {
			div *= unit
			exp++
		}
		return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGT"[exp])
	},
	"join": strings.Join,
	// rows bundles the arguments of the "buildrows" template.
	"rows": func(builds []store.Build, csrf, when string, actions bool) map[string]any {
		return map[string]any{"Builds": builds, "CSRF": csrf, "When": when, "Actions": actions}
	},
	"add": func(a, b int) int { return a + b },
}

func fmtDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	s := int(d.Seconds())
	switch {
	case s < 60:
		return fmt.Sprintf("%ds", s)
	case s < 3600:
		return fmt.Sprintf("%dm %02ds", s/60, s%60)
	}
	return fmt.Sprintf("%dh %02dm", s/3600, s%3600/60)
}
