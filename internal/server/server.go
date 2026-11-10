// Package server is FCP's HTTP server: routing, auth, rate limits, REST API.
package server

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"text/template"

	"github.com/sofriendly/family-context-protocol/internal/mcp"
	"github.com/sofriendly/family-context-protocol/internal/notify"
	"github.com/sofriendly/family-context-protocol/internal/store"
	"github.com/sofriendly/family-context-protocol/internal/ui"
)

type Config struct {
	AdminToken string // enables POST /v1/households; unset = disabled
	PublicURL  string // base for links we hand out; derived from the request when empty
	Version    string // running version, shown in /admin
	Restart    func() // called after /admin installs an update; nil disables installing
	// AllowPrivateWebhooks permits webhooks to http:// and private-network
	// addresses (FCP_WEBHOOK_ALLOW_PRIVATE=1).
	AllowPrivateWebhooks bool
}

// Contract is the operating contract for agents, served at /agents.md.
//
//go:embed agents.md
var Contract string

//go:embed join.md
var joinMD string

var joinTmpl = template.Must(template.New("join").Parse(joinMD))

// ContractVersion changes whenever the contract text does. It's returned by
// list_agents and check_inbox so connected agents know to re-read /agents.md.
var ContractVersion = func() string {
	h := sha256.Sum256([]byte(Contract))
	return hex.EncodeToString(h[:4])
}()

type server struct {
	st  *store.Store
	cfg Config
	rl  *Limiter
	n   *notify.Notifier
}

// New returns the complete handler: REST, MCP, and the admin UI.
func New(st *store.Store, cfg Config) http.Handler {
	s := &server{st: st, cfg: cfg, rl: NewLimiter(), n: notify.New(cfg.AllowPrivateWebhooks, cfg.Version)}
	st.OnDeliver = func(to store.Agent, m store.Message) {
		// Webhooks fire outside any request, so links are absolute only with --public-url.
		s.n.Deliver(to, store.WithBase([]store.Message{m}, strings.TrimRight(cfg.PublicURL, "/"))[0])
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, map[string]bool{"ok": true}) })
	mux.HandleFunc("POST /v1/households", s.createHousehold)
	mux.HandleFunc("GET /join/{token}", s.joinInfo)
	mux.HandleFunc("POST /join/{token}", s.join)
	mux.HandleFunc("GET /agents.md", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		io.WriteString(w, Contract)
	})
	mux.HandleFunc("POST /v1/invites", s.any(true, s.createInvite))
	mux.HandleFunc("GET /v1/agents", s.any(false, s.listAgents))
	mux.HandleFunc("POST /v1/agents", s.any(true, s.mintAgent))
	mux.HandleFunc("POST /v1/agents/{id}/rotate", s.any(true, s.rotateAgent))
	mux.HandleFunc("DELETE /v1/agents/{id}", s.any(true, s.deleteAgent))
	mux.HandleFunc("POST /v1/messages", s.agent(s.send(false)))
	mux.HandleFunc("POST /v1/broadcast", s.agent(s.send(true)))
	mux.HandleFunc("GET /v1/inbox", s.agent(s.inbox))
	mux.HandleFunc("/v1/", func(w http.ResponseWriter, r *http.Request) {
		WriteErr(w, 404, "no such endpoint", "endpoints: GET /v1/inbox, POST /v1/messages, POST /v1/broadcast, GET /v1/agents (see /agents.md)")
	})
	mux.HandleFunc("POST /v1/files", s.agent(s.uploadDirect))
	mux.HandleFunc("POST /v1/uploads", s.agent(s.declareUpload))
	mux.HandleFunc("PUT /v1/upload/{token}", s.putUpload)
	mux.HandleFunc("GET /v1/files/{id}", s.download)
	mux.HandleFunc("POST /v1/webhook", s.agent(s.setWebhook))
	mux.HandleFunc("POST /v1/watch", s.agent(s.newWatch))
	mux.HandleFunc("GET /v1/watch/{token}", s.watch)
	mux.Handle("/mcp", mcp.Handler(st, mcp.Options{Allow: s.rl.Allow, ContractVersion: ContractVersion, Base: s.base, ValidateWebhook: s.n.Validate}))
	mux.Handle("/", ui.Handler(st, ui.Config{PublicURL: cfg.PublicURL, Version: cfg.Version, Restart: cfg.Restart}))
	return mux
}

// ---- helpers ----

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// WriteErr writes the one error shape every caller gets.
func WriteErr(w http.ResponseWriter, status int, code, hint string) {
	writeJSON(w, status, map[string]string{"error": code, "hint": hint})
}

func storeErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		WriteErr(w, 404, "not found", "no such agent in this household; see GET /v1/agents")
	case errors.Is(err, store.ErrStorageFull):
		WriteErr(w, 507, "file storage full", "the server is holding 200 MB of undelivered files; try again after recipients check their inboxes")
	case errors.Is(err, store.ErrGone):
		WriteErr(w, 410, "gone", "this link was already used or has expired; start over")
	case errors.Is(err, store.ErrInboxFull):
		WriteErr(w, 429, "recipient inbox full", "the recipient has 1000 unread messages; wait until it checks its inbox")
	case errors.Is(err, store.ErrBadRequest):
		WriteErr(w, 400, "bad request", err.Error())
	default:
		log.Printf("internal error: %v", err)
		WriteErr(w, 500, "internal error", "the server logged the details; retry later")
	}
}

func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if len(h) > 7 && strings.EqualFold(h[:7], "bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return ""
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// caller is who a request authenticated as. agent is nil for a household key.
type caller struct {
	hid   string
	agent *store.Agent
}

func (s *server) resolve(w http.ResponseWriter, r *http.Request) (*caller, bool) {
	key := bearer(r)
	switch {
	case strings.HasPrefix(key, "fcph_"):
		if hid, err := s.st.HouseholdByKey(key); err == nil {
			return &caller{hid: hid}, true
		}
		WriteErr(w, 401, "unknown household key", "check the fcph_ key from first run (household-key.txt next to the keys file)")
	case strings.HasPrefix(key, "fcpa_"):
		if a, err := s.st.AgentByKey(key); err == nil {
			return &caller{hid: a.HouseholdID, agent: a}, true
		}
		WriteErr(w, 401, "unknown agent key", "the key may have been rotated or revoked; ask the household admin for a new fcpa_ key")
	default:
		WriteErr(w, 401, "missing key", "send Authorization: Bearer <fcpa_ agent key or fcph_ household key>")
	}
	return nil, false
}

type handler func(http.ResponseWriter, *http.Request, *caller)

// any accepts a household or agent key; admin=true requires the household key.
func (s *server) any(admin bool, h handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, ok := s.resolve(w, r)
		if !ok {
			return
		}
		if admin && c.agent != nil {
			WriteErr(w, 401, "household key required", "this is an admin route; use the fcph_ household key, not an agent key")
			return
		}
		if !s.rl.Allow(bearer(r), r.Method != http.MethodGet) {
			WriteErr(w, 429, "rate limited", "slow down: 60 reads/min and 30 writes/min per key")
			return
		}
		h(w, r, c)
	}
}

// agent requires an agent key.
func (s *server) agent(h handler) http.HandlerFunc {
	return s.any(false, func(w http.ResponseWriter, r *http.Request, c *caller) {
		if c.agent == nil {
			WriteErr(w, 401, "agent key required", "this route acts as an agent; mint one with POST /v1/agents and use its fcpa_ key")
			return
		}
		h(w, r, c)
	})
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, store.MaxBody+64<<10)) // room for the JSON envelope
	if err != nil {
		WriteErr(w, 413, "payload too large", "message bodies are capped at 256 KB")
		return false
	}
	if len(b) == 0 {
		b = []byte("{}")
	}
	if err := json.Unmarshal(b, v); err != nil {
		WriteErr(w, 400, "bad json", err.Error())
		return false
	}
	return true
}

// ---- handlers ----

// base is the public origin for links: --public-url, else what the client used.
func (s *server) base(r *http.Request) string {
	if s.cfg.PublicURL != "" {
		return strings.TrimRight(s.cfg.PublicURL, "/")
	}
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// joinInfo answers a plain GET of an invite link (what an agent, a person, or a
// link preview sees) with instructions. It never uses the invite up.
func (s *server) joinInfo(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Robots-Tag", "noindex")
	if s.st.CheckInvite(r.PathValue("token")) != nil {
		w.WriteHeader(http.StatusGone)
		io.WriteString(w, "This FCP invite link was already used or has expired.\n\nAsk the person who sent it for a new link (they can make one at the server's /admin page).\n")
		return
	}
	var buf bytes.Buffer
	joinTmpl.Execute(&buf, map[string]string{"Base": s.base(r), "Token": r.PathValue("token"), "Contract": Contract})
	w.Write(buf.Bytes())
}

// join redeems an invite: mints an agent and, for a household's first agent,
// a one-time link for their partner.
func (s *server) join(w http.ResponseWriter, r *http.Request) {
	if !s.rl.AllowN("join:"+clientIP(r), 10) {
		WriteErr(w, 429, "rate limited", "invite redemption is limited to 10/min per IP")
		return
	}
	var in struct {
		Name string `json:"name"`
	}
	if !decode(w, r, &in) {
		return
	}
	red, err := s.st.Redeem(r.PathValue("token"), in.Name)
	if errors.Is(err, store.ErrGone) {
		WriteErr(w, 410, "invite used or expired", "ask the person who sent this link for a new one; they can make one at /admin")
		return
	}
	if err != nil {
		storeErr(w, err)
		return
	}
	a, base := red.Agent, s.base(r)
	out := map[string]any{"agent_id": a.ID, "name": a.Name, "mcp_url": base + "/mcp", "api_base": base + "/v1",
		"instructions": base + "/agents.md"}
	if red.Key != "" {
		out["agent_key"] = red.Key
		out["key_link"] = base + "/key/" + s.st.CreateKeyLink(a.HouseholdID, a.ID)
	} else {
		// Set up in /admin: the person already has the key.
		out["agent_key_from_user"] = true
	}
	if tok, err := s.st.NewWatchToken(&a); err == nil {
		out["watch_url"] = base + "/v1/watch/" + tok
	}
	if red.PartnerToken != "" {
		out["partner_link"] = base + "/join/" + red.PartnerToken
		out["partner_message"] = ui.PasteMessage(base+"/join/"+red.PartnerToken, a.Name)
		out["partner_link_expires_at"] = red.PartnerExpires
	}
	writeJSON(w, 201, out)
}

func (s *server) createInvite(w http.ResponseWriter, r *http.Request, c *caller) {
	tok, exp, err := s.st.CreateInvite(c.hid)
	if err != nil {
		storeErr(w, err)
		return
	}
	writeJSON(w, 201, map[string]any{"invite_link": s.base(r) + "/join/" + tok, "expires_at": exp})
}

func (s *server) createHousehold(w http.ResponseWriter, r *http.Request) {
	if s.cfg.AdminToken == "" {
		WriteErr(w, 403, "household creation disabled", "this server has one household; set FCP_ADMIN_TOKEN to allow creating more")
		return
	}
	if subtle.ConstantTimeCompare([]byte(bearer(r)), []byte(s.cfg.AdminToken)) != 1 {
		WriteErr(w, 401, "admin token required", "this server has FCP_ADMIN_TOKEN set; send it as Authorization: Bearer <token>")
		return
	}
	if !s.rl.AllowN("ip:"+clientIP(r), 5) {
		WriteErr(w, 429, "rate limited", "household creation is limited to 5/min per IP")
		return
	}
	id, key, err := s.st.CreateHousehold()
	if err != nil {
		storeErr(w, err)
		return
	}
	writeJSON(w, 201, map[string]string{"household_id": id, "household_key": key})
}

func (s *server) listAgents(w http.ResponseWriter, r *http.Request, c *caller) {
	as, err := s.st.ListAgents(c.hid)
	if err != nil {
		storeErr(w, err)
		return
	}
	out := map[string]any{"agents": as, "contract_version": ContractVersion}
	if c.agent != nil {
		out["you"] = c.agent.ID
	}
	writeJSON(w, 200, out)
}

func (s *server) mintAgent(w http.ResponseWriter, r *http.Request, c *caller) {
	var in struct {
		Name string `json:"name"`
	}
	if !decode(w, r, &in) {
		return
	}
	a, key, err := s.st.MintAgent(c.hid, in.Name)
	if err != nil {
		storeErr(w, err)
		return
	}
	writeJSON(w, 201, map[string]string{"agent_key": key, "agent_id": a.ID})
}

func (s *server) rotateAgent(w http.ResponseWriter, r *http.Request, c *caller) {
	key, err := s.st.RotateAgent(c.hid, r.PathValue("id"))
	if err != nil {
		storeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]string{"agent_key": key, "agent_id": r.PathValue("id")})
}

func (s *server) deleteAgent(w http.ResponseWriter, r *http.Request, c *caller) {
	if err := s.st.DeleteAgent(c.hid, r.PathValue("id")); err != nil {
		storeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"deleted": true})
}

// send is POST /v1/messages (one recipient, required) or, with all,
// POST /v1/broadcast (every other agent, no recipient field).
func (s *server) send(all bool) handler {
	return func(w http.ResponseWriter, r *http.Request, c *caller) {
		var in struct {
			To          *string  `json:"to_agent_id"`
			Kind        string   `json:"kind"`
			Body        string   `json:"body"`
			Re          *int64   `json:"re"`
			Attachments []string `json:"attachments"`
		}
		if !decode(w, r, &in) {
			return
		}
		var (
			m   store.Message
			to  []string
			err error
		)
		switch {
		case all && in.To != nil:
			WriteErr(w, 400, "bad request", "a broadcast goes to every other agent; drop to_agent_id, or use POST /v1/messages for one agent")
			return
		case all:
			m, to, err = s.st.Broadcast(c.agent, in.Kind, in.Body, in.Re, in.Attachments...)
		default:
			var id string
			if in.To != nil {
				id = *in.To
			}
			m, to, err = s.st.Send(c.agent, id, in.Kind, in.Body, in.Re, in.Attachments...)
		}
		if err != nil {
			storeErr(w, err)
			return
		}
		writeJSON(w, 201, map[string]any{"id": m.ID, "delivered_to": to, "created_at": m.CreatedAt})
	}
}

func (s *server) inbox(w http.ResponseWriter, r *http.Request, c *caller) {
	var after int64
	if v := r.URL.Query().Get("after"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			WriteErr(w, 400, "bad after", "after is the id of the last message you handled; omit it on first call")
			return
		}
		after = n
	}
	writeJSON(w, 200, map[string]any{"messages": store.WithBase(s.st.Inbox(c.agent, after), s.base(r)), "contract_version": ContractVersion})
}

func (s *server) setWebhook(w http.ResponseWriter, r *http.Request, c *caller) {
	var in struct {
		URL string `json:"url"`
	}
	if !decode(w, r, &in) {
		return
	}
	if in.URL != "" {
		if err := s.n.Validate(in.URL); err != nil {
			WriteErr(w, 400, "bad webhook url", err.Error())
			return
		}
	}
	secret, err := s.st.SetWebhook(c.agent, in.URL)
	if err != nil {
		storeErr(w, err)
		return
	}
	if in.URL == "" {
		writeJSON(w, 200, map[string]any{"webhook_url": nil})
		return
	}
	writeJSON(w, 200, map[string]any{"webhook_url": in.URL, "secret": secret})
}

func (s *server) newWatch(w http.ResponseWriter, r *http.Request, c *caller) {
	tok, err := s.st.NewWatchToken(c.agent)
	if err != nil {
		storeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"watch_url": s.base(r) + "/v1/watch/" + tok})
}

// watch answers a watch URL: no key needed, and only a count, never content.
func (s *server) watch(w http.ResponseWriter, r *http.Request) {
	tok := r.PathValue("token")
	if !s.rl.AllowN("watch:"+store.Hash(tok), 120) {
		WriteErr(w, 429, "rate limited", "poll a watch URL at most 120 times a minute")
		return
	}
	waiting, newest, err := s.st.Watch(tok)
	if err != nil {
		WriteErr(w, 404, "unknown watch url", "get a new one with get_watch_url (or POST /v1/watch)")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, map[string]any{"waiting": waiting, "newest_id": newest})
}

// ---- attachments ----

func attachmentJSON(a store.Attachment, base string) map[string]any {
	return map[string]any{"file_id": a.ID, "name": a.Name, "content_type": a.ContentType, "size": a.Size, "download_url": base + a.DownloadURL}
}

// uploadDirect stores the request body as a file in one step: POST /v1/files?name=plan.pdf
func (s *server) uploadDirect(w http.ResponseWriter, r *http.Request, c *caller) {
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, store.MaxFile))
	if err != nil {
		WriteErr(w, 413, "file too large", "files are capped at 25 MB")
		return
	}
	a, err := s.st.AddFile(c.agent, r.URL.Query().Get("name"), r.Header.Get("Content-Type"), data)
	if err != nil {
		storeErr(w, err)
		return
	}
	writeJSON(w, 201, attachmentJSON(a, s.base(r)))
}

// declareUpload returns a one-time URL to PUT the bytes to, for agents whose
// HTTP tool can't send an auth header: POST /v1/uploads {name, content_type, size}
func (s *server) declareUpload(w http.ResponseWriter, r *http.Request, c *caller) {
	var in struct {
		Name        string `json:"name"`
		ContentType string `json:"content_type"`
		Size        int64  `json:"size"`
	}
	if !decode(w, r, &in) {
		return
	}
	id, tok, err := s.st.NewUpload(c.agent, in.Name, in.ContentType, in.Size)
	if err != nil {
		storeErr(w, err)
		return
	}
	writeJSON(w, 201, map[string]any{"file_id": id, "upload_url": s.base(r) + "/v1/upload/" + tok, "upload_method": "PUT", "expires_in_seconds": 900})
}

func (s *server) putUpload(w http.ResponseWriter, r *http.Request) {
	if !s.rl.AllowN("upload:"+clientIP(r), 30) {
		WriteErr(w, 429, "rate limited", "at most 30 uploads a minute")
		return
	}
	tok := r.PathValue("token")
	size, err := s.st.UploadSize(tok)
	if err != nil {
		storeErr(w, err)
		return
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, size))
	if err != nil {
		WriteErr(w, 413, "too many bytes", "send exactly the size you declared")
		return
	}
	a, err := s.st.PutUpload(tok, data)
	if err != nil {
		storeErr(w, err)
		return
	}
	writeJSON(w, 200, attachmentJSON(a, s.base(r)))
}

// download serves an attachment. The file id is the capability (no key), and
// the bytes are always served as a download so they can't run as a page here.
func (s *server) download(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.rl.AllowN("dl:"+store.Hash(id), 60) {
		WriteErr(w, 429, "rate limited", "at most 60 downloads of a file a minute")
		return
	}
	a, data, err := s.st.File(id)
	if err != nil {
		WriteErr(w, 404, "file not found", "it was already delivered to everyone and deleted, or it expired")
		return
	}
	h := w.Header()
	h.Set("Content-Type", "application/octet-stream")
	h.Set("X-FCP-Content-Type", a.ContentType)
	h.Set("Content-Disposition", "attachment; filename*=UTF-8''"+url.PathEscape(a.Name))
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "sandbox; default-src 'none'")
	h.Set("Cache-Control", "private, no-store")
	w.Write(data)
}

// ReleaseNote is what agents are told when a server is updated to this
// release (once per contract change). Update it with each release.
const ReleaseNote = "Changed: send_message (POST /v1/messages) now requires to_agent_id; leaving it out is an error instead of sending to everyone. To message every other agent, use the new broadcast_message tool (POST /v1/broadcast). If you use an MCP tool allowlist, add broadcast_message to it."

// AnnounceRelease posts the release note to every household whose agents
// haven't heard about the current contract yet.
func AnnounceRelease(st *store.Store, version, base string) error {
	return st.AnnounceRelease(ContractVersion, fmt.Sprintf("FCP was updated to %s. %s Please re-read the rules at %s/agents.md.", version, ReleaseNote, base))
}
