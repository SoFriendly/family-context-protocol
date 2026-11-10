// Package ui is the small server-rendered admin page, behind the household key.
// It shows agents and how many messages wait for each, never message content.
package ui

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"html/template"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/sofriendly/family-context-protocol/internal/store"
	"github.com/sofriendly/family-context-protocol/internal/update"
)

//go:embed templates/*.html
var files embed.FS

var tmpl = template.Must(template.New("").ParseFS(files, "templates/*.html"))

const cookieName = "fcp_admin"

// Config is what the admin UI needs from the running server.
type Config struct {
	PublicURL string // --public-url, if set
	Version   string
	Restart   func() // restarts into a newly installed binary; nil disables installing
}

type ui struct {
	st         *store.Store
	secret     []byte
	public     string
	cfg        Config
	installing sync.Mutex
}

// Handler serves "/" and "/admin…". The cookie secret lives in memory, so a
// restart signs everyone out.
func Handler(st *store.Store, cfg Config) http.Handler {
	u := &ui{st: st, secret: make([]byte, 32), public: strings.TrimRight(cfg.PublicURL, "/"), cfg: cfg}
	rand.Read(u.secret)
	mux := http.NewServeMux()
	mux.Handle("GET /{$}", http.RedirectHandler("/admin", http.StatusSeeOther))
	mux.HandleFunc("GET /admin", u.admin)
	mux.HandleFunc("POST /admin/login", u.login)
	mux.HandleFunc("POST /admin/logout", u.logout)
	mux.HandleFunc("POST /admin/agents/{id}/rotate", u.needAdmin(u.rotate))
	mux.HandleFunc("POST /admin/agents/{id}/delete", u.needAdmin(u.revoke))
	mux.HandleFunc("POST /admin/invites", u.needAdmin(u.invite))
	mux.HandleFunc("POST /admin/setup", u.needAdmin(u.setup))
	mux.HandleFunc("POST /admin/update", u.needAdmin(u.checkUpdate))
	mux.HandleFunc("POST /admin/update/install", u.needAdmin(u.installUpdate))
	mux.HandleFunc("GET /key/{token}", u.keyPage)
	mux.HandleFunc("POST /key/{token}", u.revealKey)
	return mux
}

func (u *ui) sign(hid string) string {
	m := hmac.New(sha256.New, u.secret)
	m.Write([]byte(hid))
	return hid + "." + hex.EncodeToString(m.Sum(nil))
}

// session returns the household id from a valid signed cookie, or "".
func (u *ui) session(r *http.Request) string {
	c, err := r.Cookie(cookieName)
	if err != nil {
		return ""
	}
	hid, _, _ := strings.Cut(c.Value, ".")
	if hmac.Equal([]byte(c.Value), []byte(u.sign(hid))) {
		return hid
	}
	return ""
}

func (u *ui) render(w http.ResponseWriter, name string, data map[string]any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; img-src data:; form-action 'self'")
	w.Header().Set("Cache-Control", "no-store")
	if err := tmpl.ExecuteTemplate(w, name, data); err != nil {
		log.Printf("ui: %v", err)
	}
}

func (u *ui) admin(w http.ResponseWriter, r *http.Request) {
	hid := u.session(r)
	if hid == "" {
		u.render(w, "login.html", map[string]any{"Host": r.Host})
		return
	}
	u.page(w, hid, nil)
}

func (u *ui) page(w http.ResponseWriter, hid string, shown map[string]string) {
	u.pageWith(w, hid, map[string]any{"Shown": shown})
}

func (u *ui) pageWith(w http.ResponseWriter, hid string, data map[string]any) {
	agents, err := u.st.ListAgents(hid)
	if err != nil {
		http.Error(w, "household not found", 404)
		return
	}
	waiting := 0
	for _, a := range agents {
		waiting += a.Pending
	}
	data["Agents"], data["Waiting"], data["Version"] = agents, waiting, u.cfg.Version
	u.render(w, "admin.html", data)
}

// checkUpdate asks getfcp.com for the latest release (only when the button is pressed).
func (u *ui) checkUpdate(w http.ResponseWriter, r *http.Request, hid string) {
	upd := map[string]any{"Current": u.cfg.Version}
	latest, err := update.Latest()
	if err != nil {
		upd["Error"] = "Couldn't check for updates: " + err.Error()
	} else {
		upd["Latest"], upd["Newer"], upd["CanInstall"] = latest, update.Newer(u.cfg.Version, latest), u.cfg.Restart != nil
	}
	u.pageWith(w, hid, map[string]any{"Update": upd})
}

// installUpdate downloads and verifies the latest release, replaces this
// binary, and restarts into it. Undelivered messages are lost on restart; the
// page warns before the button is pressed.
func (u *ui) installUpdate(w http.ResponseWriter, r *http.Request, hid string) {
	fail := func(msg string) {
		u.pageWith(w, hid, map[string]any{"Update": map[string]any{"Current": u.cfg.Version, "Error": msg}})
	}
	if u.cfg.Restart == nil {
		fail("This server can't restart itself. Run `fcp update` and restart it by hand.")
		return
	}
	if !u.installing.TryLock() {
		fail("An update is already being installed.")
		return
	}
	defer u.installing.Unlock()
	latest, err := update.Latest()
	if err != nil {
		fail("Couldn't check for updates: " + err.Error())
		return
	}
	if !update.Newer(u.cfg.Version, latest) {
		fail("Already up to date (" + u.cfg.Version + ").")
		return
	}
	exe, err := update.Executable()
	if err == nil {
		err = update.Install(latest, exe)
	}
	if err != nil {
		fail("Update failed, nothing was changed: " + err.Error())
		return
	}
	log.Printf("admin installed %s; restarting", latest)
	u.render(w, "restarting.html", map[string]any{"Version": latest})
	time.AfterFunc(300*time.Millisecond, u.cfg.Restart)
}

func (u *ui) login(w http.ResponseWriter, r *http.Request) {
	hid, err := u.st.HouseholdByKey(strings.TrimSpace(r.FormValue("key")))
	if err != nil {
		w.WriteHeader(401)
		u.render(w, "login.html", map[string]any{"Host": r.Host, "Error": "That household key was not recognized."})
		return
	}
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: u.sign(hid), Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode,
		Secure: r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https"})
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func (u *ui) logout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteLaxMode})
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func (u *ui) needAdmin(h func(http.ResponseWriter, *http.Request, string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		hid := u.session(r)
		if hid == "" {
			http.Redirect(w, r, "/admin", http.StatusSeeOther)
			return
		}
		h(w, r, hid)
	}
}

// rotate answers with the page itself (no redirect) so the new key is shown
// exactly once and never stored.
func (u *ui) rotate(w http.ResponseWriter, r *http.Request, hid string) {
	key, err := u.st.RotateAgent(hid, r.PathValue("id"))
	if err != nil {
		http.Error(w, "agent not found", 404)
		return
	}
	name := ""
	agents, _ := u.st.ListAgents(hid)
	for _, a := range agents {
		if a.ID == r.PathValue("id") {
			name = a.Name
		}
	}
	u.page(w, hid, map[string]string{"Name": name, "Key": key, "Verb": "Rotated"})
}

func (u *ui) revoke(w http.ResponseWriter, r *http.Request, hid string) {
	if err := u.st.DeleteAgent(hid, r.PathValue("id")); err != nil {
		http.Error(w, "agent not found", 404)
		return
	}
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func (u *ui) base(r *http.Request) string {
	if u.public != "" {
		return u.public
	}
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// setup mints an agent for an assistant the admin is setting up, and shows
// both the paste-into-chat setup message and the key for its secure prompt.
func (u *ui) setup(w http.ResponseWriter, r *http.Request, hid string) {
	a, key, tok, exp, err := u.st.SetUpAgent(hid, r.FormValue("name"))
	if errors.Is(err, store.ErrBadRequest) {
		http.Error(w, "name is required (1-200 characters)", 400)
		return
	}
	if err != nil {
		log.Printf("ui: %v", err)
		http.Error(w, "internal error", 500)
		return
	}
	u.pageWith(w, hid, map[string]any{"Setup": map[string]string{
		"Name": a.Name, "Key": key, "Message": PasteMessage(u.base(r)+"/join/"+tok, ""), "Expires": exp.Format("Jan 2")}})
}

// invite shows a fresh one-time setup link, once.
func (u *ui) invite(w http.ResponseWriter, r *http.Request, hid string) {
	tok, exp, err := u.st.CreateInvite(hid)
	if err != nil {
		log.Printf("ui: %v", err)
		http.Error(w, "internal error", 500)
		return
	}
	base := u.public
	if base == "" {
		scheme := "http"
		if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
			scheme = "https"
		}
		base = scheme + "://" + r.Host
	}
	u.page(w, hid, map[string]string{"Link": PasteMessage(base+"/join/"+tok, ""), "Expires": exp.Format("Jan 2")})
}

// keyPage is the one-time key link a person opens for their assistant. GET only
// shows a button, so link previews can't use it up.
func (u *ui) keyPage(w http.ResponseWriter, r *http.Request) {
	name, err := u.st.KeyLinkAgent(r.PathValue("token"))
	if err != nil {
		w.WriteHeader(http.StatusGone)
	}
	u.render(w, "key.html", map[string]any{"Name": name, "Gone": err != nil})
}

func (u *ui) revealKey(w http.ResponseWriter, r *http.Request) {
	name, key, err := u.st.RevealKey(r.PathValue("token"))
	if err != nil {
		w.WriteHeader(http.StatusGone)
		u.render(w, "key.html", map[string]any{"Gone": true})
		return
	}
	u.render(w, "key.html", map[string]any{"Name": name, "Key": key})
}
