// Package store holds FCP's state: agent credentials (hashes only, in a small
// JSON file) and undelivered messages (memory only, deleted once read).
package store

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var (
	ErrNotFound   = errors.New("not found")
	ErrBadRequest = errors.New("bad request")
	ErrInboxFull  = errors.New("recipient inbox full")
	ErrGone       = errors.New("invite used, expired, or unknown")
)

const (
	// MaxBody caps a message body.
	MaxBody = 256 << 10
	// MaxInbox caps undelivered messages per agent.
	// ponytail: fixed cap, 1000 x 256 KB worst case per agent; make it a flag if it ever bites.
	MaxInbox = 1000
	// TTL drops messages nobody picked up, and is how long an invite link lasts.
	TTL = 7 * 24 * time.Hour
)

// Kinds agents may send. The server alone sends SystemKind.
var Kinds = []string{"update", "question", "task", "notice", "reply"}

const SystemKind = "system"

type Agent struct {
	ID          string `json:"id"`
	HouseholdID string `json:"-"`
	Name        string `json:"name"`
	KeyHash     string `json:"key_hash"`
	CreatedAt   string `json:"created_at"`
	// Optional delivery nudges (config, not content).
	WebhookURL    string `json:"webhook_url,omitempty"`
	WebhookSecret string `json:"webhook_secret,omitempty"` // signs webhook bodies (HMAC), so stored as-is
	WatchHash     string `json:"watch_hash,omitempty"`     // hash of the agent's watch-URL token
}

type household struct {
	ID        string    `json:"id"`
	KeyHash   string    `json:"key_hash"`
	CreatedAt string    `json:"created_at"`
	Agents    []*Agent  `json:"agents"`
	Invites   []*invite `json:"invites,omitempty"`
	// LastContract is the contract version the household's agents were last
	// told about, so an update that changes it is announced once.
	LastContract string `json:"last_contract,omitempty"`
}

// invite is a one-time link that lets an agent join a household.
type invite struct {
	Hash    string    `json:"hash"`
	Expires time.Time `json:"expires"`
	AgentID string    `json:"agent_id,omitempty"` // set: connects this existing agent instead of minting one
}

type Message struct {
	ID          int64        `json:"id"`
	From        *string      `json:"from_agent_id"` // nil for system messages from the server
	FromName    string       `json:"from_name"`
	To          *string      `json:"to_agent_id"` // nil = sent to every other agent
	Kind        string       `json:"kind"`
	Body        string       `json:"body"`
	Re          *int64       `json:"re"` // id of the message this answers
	Attachments []Attachment `json:"attachments,omitempty"`
	CreatedAt   string       `json:"created_at"`
	created     time.Time
}

// Store is safe for concurrent use.
// ponytail: one global lock; fine for households, per-household locks if it ever isn't.
type Store struct {
	mu     sync.Mutex
	path   string
	hh     []*household
	inbox  map[string][]Message // agent id -> undelivered, oldest first
	nextID int64
	// Memory only: recent redemptions (so a retried join isn't a dead end)
	// and one-time key links (token hash -> agent).
	recent    map[string]recentJoin
	keyLinks  map[string]keyLink
	files     map[string]*file // memory only; see files.go
	fileBytes int64

	// OnDeliver, if set, is called (outside the lock) for each recipient of a
	// new message, e.g. to fire its webhook.
	OnDeliver func(recipient Agent, m Message)
}

type recentJoin struct {
	hid, aid, partner string
	partnerExp, until time.Time
	bound             bool
}

type keyLink struct {
	hid, aid string
	until    time.Time
}

const (
	retryWindow = 15 * time.Minute
	keyLinkTTL  = 24 * time.Hour
)

// Open loads (or creates) the credentials file. created reports a new file.
func Open(path string) (s *Store, created bool, err error) {
	// Message ids start at boot time in microseconds, so ids never repeat across
	// restarts and a cursor from before a restart can't delete newer messages.
	s = &Store{path: path, inbox: map[string][]Message{}, nextID: time.Now().UnixMicro(),
		recent: map[string]recentJoin{}, keyLinks: map[string]keyLink{}, files: map[string]*file{}}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, true, s.save()
	}
	if err != nil {
		return nil, false, err
	}
	if err := json.Unmarshal(b, &s.hh); err != nil {
		return nil, false, fmt.Errorf("%s: %w", path, err)
	}
	for _, h := range s.hh {
		for _, a := range h.Agents {
			a.HouseholdID = h.ID
		}
	}
	return s, false, nil
}

// save writes the credentials atomically, 0600. Caller holds mu (or is Open).
func (s *Store) save() error {
	if s.hh == nil {
		s.hh = []*household{}
	}
	b, err := json.MarshalIndent(s.hh, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".fcp-keys-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), s.path)
}

func Now() string { return time.Now().UTC().Format(time.RFC3339Nano) }

func randHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// Hash is the at-rest form of a key.
func Hash(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func (s *Store) household(id string) *household {
	for _, h := range s.hh {
		if h.ID == id {
			return h
		}
	}
	return nil
}

// ---- credentials ----

func (s *Store) CreateHousehold() (id, key string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	h := &household{ID: randHex(16), CreatedAt: Now(), Agents: []*Agent{}}
	key = "fcph_" + randHex(32)
	h.KeyHash = Hash(key)
	s.hh = append(s.hh, h)
	if err := s.save(); err != nil {
		s.hh = s.hh[:len(s.hh)-1]
		return "", "", err
	}
	return h.ID, key, nil
}

func (s *Store) HouseholdByKey(key string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	hash := Hash(key)
	for _, h := range s.hh {
		if h.KeyHash == hash {
			return h.ID, nil
		}
	}
	return "", ErrNotFound
}

// AgentByKey returns a copy of the agent holding key.
func (s *Store) AgentByKey(key string) (*Agent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	hash := Hash(key)
	for _, h := range s.hh {
		for _, a := range h.Agents {
			if a.KeyHash == hash {
				c := *a
				return &c, nil
			}
		}
	}
	return nil, ErrNotFound
}

// AgentInfo is what other agents and the admin see about an agent.
type AgentInfo struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	CreatedAt string `json:"created_at"`
	Pending   int    `json:"pending"` // undelivered messages in its inbox
	// Attachments on those messages: a count and total size, never names or content.
	PendingFiles     int   `json:"pending_files"`
	PendingFileBytes int64 `json:"pending_file_bytes"`
}

func (s *Store) ListAgents(hid string) ([]AgentInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	h := s.household(hid)
	if h == nil {
		return nil, ErrNotFound
	}
	s.expire()
	out := []AgentInfo{}
	for _, a := range h.Agents {
		info := AgentInfo{ID: a.ID, Name: a.Name, CreatedAt: a.CreatedAt, Pending: len(s.inbox[a.ID])}
		for _, m := range s.inbox[a.ID] {
			for _, f := range m.Attachments {
				info.PendingFiles++
				info.PendingFileBytes += f.Size
			}
		}
		out = append(out, info)
	}
	return out, nil
}

// MintAgent creates an agent and tells the household's other agents it joined.
func (s *Store) MintAgent(hid, name string) (a Agent, key string, err error) {
	if a, key, err = s.mintAgent(hid, name); err == nil {
		s.postSystem(hid, a.ID, fmt.Sprintf("%s joined the household (agent id %s). You can now send it messages.", a.Name, a.ID))
	}
	return a, key, err
}

func (s *Store) mintAgent(hid, name string) (a Agent, key string, err error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 200 {
		return a, "", fmt.Errorf("%w: name is required (1-200 chars)", ErrBadRequest)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	h := s.household(hid)
	if h == nil {
		return a, "", ErrNotFound
	}
	key = "fcpa_" + randHex(32)
	rec := &Agent{ID: randHex(16), HouseholdID: hid, Name: name, KeyHash: Hash(key), CreatedAt: Now()}
	h.Agents = append(h.Agents, rec)
	if err := s.save(); err != nil {
		h.Agents = h.Agents[:len(h.Agents)-1]
		return a, "", err
	}
	return *rec, key, nil
}

func (s *Store) agent(hid, aid string) (*household, int) {
	h := s.household(hid)
	if h == nil {
		return nil, -1
	}
	for i, a := range h.Agents {
		if a.ID == aid {
			return h, i
		}
	}
	return h, -1
}

// RotateAgent issues a new key; the old one stops working immediately.
func (s *Store) RotateAgent(hid, aid string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	h, i := s.agent(hid, aid)
	if i < 0 {
		return "", ErrNotFound
	}
	key := "fcpa_" + randHex(32)
	old := h.Agents[i].KeyHash
	h.Agents[i].KeyHash = Hash(key)
	if err := s.save(); err != nil {
		h.Agents[i].KeyHash = old
		return "", err
	}
	return key, nil
}

// DeleteAgent revokes an agent, drops its inbox, and tells the others.
func (s *Store) DeleteAgent(hid, aid string) error {
	s.mu.Lock()
	h, i := s.agent(hid, aid)
	if i < 0 {
		s.mu.Unlock()
		return ErrNotFound
	}
	name := h.Agents[i].Name
	before := h.Agents
	h.Agents = append(h.Agents[:i:i], h.Agents[i+1:]...)
	if err := s.save(); err != nil {
		h.Agents = before
		s.mu.Unlock()
		return err
	}
	s.release(s.inbox[aid])
	delete(s.inbox, aid)
	s.mu.Unlock()
	s.postSystem(hid, "", fmt.Sprintf("%s was removed from the household (agent id %s). Messages to it will now fail.", name, aid))
	return nil
}

// ---- invites ----

// CreateInvite returns a one-time invite token for the household, valid for TTL.
func (s *Store) CreateInvite(hid string) (token string, expires time.Time, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	h := s.household(hid)
	if h == nil {
		return "", time.Time{}, ErrNotFound
	}
	token, expires = "fcpi_"+randHex(24), time.Now().Add(TTL).UTC().Truncate(time.Second)
	before := h.Invites
	h.Invites = append(live(h.Invites), &invite{Hash: Hash(token), Expires: expires})
	if err := s.save(); err != nil {
		h.Invites = before
		return "", time.Time{}, err
	}
	return token, expires, nil
}

func live(in []*invite) []*invite {
	out := []*invite{}
	for _, i := range in {
		if time.Now().Before(i.Expires) {
			out = append(out, i)
		}
	}
	return out
}

// findInvite returns the household and index of a live invite. Caller holds mu.
func (s *Store) findInvite(token string) (*household, int) {
	hash := Hash(token)
	for _, h := range s.hh {
		for i, inv := range h.Invites {
			if inv.Hash == hash && time.Now().Before(inv.Expires) {
				return h, i
			}
		}
	}
	return nil, -1
}

// CheckInvite reports whether token is a live invite, without using it up.
func (s *Store) CheckInvite(token string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, i := s.findInvite(token); i < 0 {
		return ErrGone
	}
	return nil
}

// Redemption is the result of joining with an invite.
type Redemption struct {
	Agent          Agent
	Key            string // "" for an agent set up in /admin: its person already has the key
	PartnerToken   string // a new invite, only when this is the household's first agent
	PartnerExpires time.Time
}

// Redeem joins with an invite. Repeating the same redemption within
// retryWindow (a response lost to a timeout) returns the same agent with a
// fresh key, instead of a dead "invite used".
func (s *Store) Redeem(token, name string) (Redemption, error) {
	a, key, first, err := s.redeemInvite(token, name)
	if errors.Is(err, ErrGone) {
		s.mu.Lock()
		r, ok := s.recent[Hash(token)]
		s.mu.Unlock()
		if !ok || time.Now().After(r.until) {
			return Redemption{}, ErrGone
		}
		if r.bound { // nothing to re-issue: the person has the key
			a := s.agentCopy(r.hid, r.aid)
			if a.ID == "" {
				return Redemption{}, ErrGone
			}
			return Redemption{a, "", r.partner, r.partnerExp}, nil
		}
		key, err := s.RotateAgent(r.hid, r.aid)
		if err != nil {
			return Redemption{}, ErrGone // revoked meanwhile
		}
		return Redemption{s.agentCopy(r.hid, r.aid), key, r.partner, r.partnerExp}, nil
	}
	if err != nil {
		return Redemption{}, err
	}
	red := Redemption{Agent: a, Key: key}
	if first {
		if red.PartnerToken, red.PartnerExpires, err = s.CreateInvite(a.HouseholdID); err != nil {
			return Redemption{}, err
		}
	}
	s.mu.Lock()
	now := time.Now()
	for k, r := range s.recent {
		if now.After(r.until) {
			delete(s.recent, k)
		}
	}
	s.recent[Hash(token)] = recentJoin{a.HouseholdID, a.ID, red.PartnerToken, red.PartnerExpires, now.Add(retryWindow), key == ""}
	s.mu.Unlock()
	return red, nil
}

func (s *Store) agentCopy(hid, aid string) Agent {
	s.mu.Lock()
	defer s.mu.Unlock()
	if h, i := s.agent(hid, aid); i >= 0 {
		return *h.Agents[i]
	}
	return Agent{}
}

// CreateKeyLink returns a one-time token that lets a person reveal a fresh key
// for an agent (for assistants that can't store a key from a response).
// Memory only; valid for keyLinkTTL.
func (s *Store) CreateKeyLink(hid, aid string) string {
	tok := "fcpk_" + randHex(24)
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for k, l := range s.keyLinks {
		if now.After(l.until) {
			delete(s.keyLinks, k)
		}
	}
	s.keyLinks[Hash(tok)] = keyLink{hid, aid, now.Add(keyLinkTTL)}
	return tok
}

// KeyLinkAgent names the agent a live key link is for, without using it.
func (s *Store) KeyLinkAgent(token string) (string, error) {
	s.mu.Lock()
	l, ok := s.keyLinks[Hash(token)]
	s.mu.Unlock()
	if !ok || time.Now().After(l.until) {
		return "", ErrGone
	}
	a := s.agentCopy(l.hid, l.aid)
	if a.ID == "" {
		return "", ErrGone
	}
	return a.Name, nil
}

// RevealKey uses up a key link: it rotates the agent's key and returns the new one.
func (s *Store) RevealKey(token string) (name, key string, err error) {
	s.mu.Lock()
	l, ok := s.keyLinks[Hash(token)]
	delete(s.keyLinks, Hash(token))
	s.mu.Unlock()
	if !ok || time.Now().After(l.until) {
		return "", "", ErrGone
	}
	if key, err = s.RotateAgent(l.hid, l.aid); err != nil {
		return "", "", ErrGone
	}
	return s.agentCopy(l.hid, l.aid).Name, key, nil
}

// redeemInvite uses up the invite and mints an agent with it. first reports
// whether this is the household's first agent (so it should invite a partner).
func (s *Store) redeemInvite(token, name string) (a Agent, key string, first bool, err error) {
	name = strings.TrimSpace(name)
	if len(name) > 200 {
		return a, "", false, fmt.Errorf("%w: name is at most 200 chars", ErrBadRequest)
	}
	s.mu.Lock()
	h, i := s.findInvite(token)
	if i < 0 {
		s.mu.Unlock()
		return a, "", false, ErrGone
	}
	if name == "" {
		name = fmt.Sprintf("Agent %d", len(h.Agents)+1)
	}
	bound := h.Invites[i].AgentID
	if bound != "" {
		if _, j := s.agent(h.ID, bound); j < 0 {
			s.mu.Unlock()
			return a, "", false, ErrGone // its agent was revoked
		}
	}
	before := h.Invites
	h.Invites = live(append(h.Invites[:i:i], h.Invites[i+1:]...))
	first = len(h.Agents) == 0 || (bound != "" && len(h.Agents) == 1)
	if err := s.save(); err != nil {
		h.Invites = before
		s.mu.Unlock()
		return a, "", false, err
	}
	s.mu.Unlock()
	if bound != "" {
		return s.agentCopy(h.ID, bound), "", first, nil
	}
	a, key, err = s.MintAgent(h.ID, name)
	return a, key, first, err
}

// SetUpAgent mints an agent and a setup invite bound to it, for /admin: the
// person gets the key (for the assistant's secure prompt) and a setup link
// that connects this agent without minting another key.
func (s *Store) SetUpAgent(hid, name string) (a Agent, key, token string, expires time.Time, err error) {
	if a, key, err = s.MintAgent(hid, name); err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	h := s.household(hid)
	token, expires = "fcpi_"+randHex(24), time.Now().Add(TTL).UTC().Truncate(time.Second)
	h.Invites = append(live(h.Invites), &invite{Hash: Hash(token), Expires: expires, AgentID: a.ID})
	err = s.save()
	return
}

// EmptyHouseholds lists households with no agents yet (first run, or everyone revoked).
func (s *Store) EmptyHouseholds() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, h := range s.hh {
		if len(h.Agents) == 0 {
			out = append(out, h.ID)
		}
	}
	return out
}

// ---- notifications ----

// SetWebhook sets (or, with url "", removes) the agent's webhook and returns
// a new signing secret. The caller validates url.
func (s *Store) SetWebhook(a *Agent, url string) (secret string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	h, i := s.agent(a.HouseholdID, a.ID)
	if i < 0 {
		return "", ErrNotFound
	}
	rec := h.Agents[i]
	oldURL, oldSecret := rec.WebhookURL, rec.WebhookSecret
	rec.WebhookURL, rec.WebhookSecret = url, ""
	if url != "" {
		secret = "fcps_" + randHex(24)
		rec.WebhookSecret = secret
	}
	if err := s.save(); err != nil {
		rec.WebhookURL, rec.WebhookSecret = oldURL, oldSecret
		return "", err
	}
	return secret, nil
}

// NewWatchToken issues the agent a new watch-URL token; the previous one stops working.
func (s *Store) NewWatchToken(a *Agent) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	h, i := s.agent(a.HouseholdID, a.ID)
	if i < 0 {
		return "", ErrNotFound
	}
	tok := "fcpw_" + randHex(24)
	old := h.Agents[i].WatchHash
	h.Agents[i].WatchHash = Hash(tok)
	if err := s.save(); err != nil {
		h.Agents[i].WatchHash = old
		return "", err
	}
	return tok, nil
}

// Watch reports how many messages wait for the agent a watch token belongs to,
// and the newest one's id. No content.
func (s *Store) Watch(token string) (waiting int, newest int64, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	hash := Hash(token)
	for _, h := range s.hh {
		for _, a := range h.Agents {
			if a.WatchHash != "" && a.WatchHash == hash {
				s.expire()
				box := s.inbox[a.ID]
				if len(box) > 0 {
					newest = box[len(box)-1].ID
				}
				return len(box), newest, nil
			}
		}
	}
	return 0, 0, ErrNotFound
}

// ---- messages ----

// Send delivers a message to one agent's inbox, or to every other agent's when
// to is nil, with optional attachments (file ids the sender uploaded). It
// returns the message and who it was delivered to.
func (s *Store) Send(from *Agent, to *string, kind, body string, re *int64, attachments ...string) (Message, []string, error) {
	m, rcpts, err := s.send(from, to, kind, body, re, attachments)
	if err == nil {
		s.notify(from.HouseholdID, rcpts, m)
	}
	return m, rcpts, err
}

func (s *Store) notify(hid string, rcpts []string, m Message) {
	if s.OnDeliver == nil {
		return
	}
	for _, r := range rcpts {
		if a := s.agentCopy(hid, r); a.ID != "" {
			s.OnDeliver(a, m)
		}
	}
}

// postSystem delivers a server-authored message to every agent in the
// household except one ("" for none). Best effort: a full inbox is skipped.
func (s *Store) postSystem(hid, except, body string) {
	s.mu.Lock()
	h := s.household(hid)
	if h == nil {
		s.mu.Unlock()
		return
	}
	s.nextID++
	now := time.Now()
	m := Message{ID: s.nextID, FromName: "FCP", Kind: SystemKind, Body: body, CreatedAt: now.UTC().Format(time.RFC3339Nano), created: now}
	var rcpts []string
	for _, a := range h.Agents {
		if a.ID != except && len(s.inbox[a.ID]) < MaxInbox {
			s.inbox[a.ID] = append(s.inbox[a.ID], m)
			rcpts = append(rcpts, a.ID)
		}
	}
	s.mu.Unlock()
	s.notify(hid, rcpts, m)
}

// AnnounceRelease tells every household's agents about a release whose
// contract differs from the one they were last told about. Once per change.
func (s *Store) AnnounceRelease(contractVersion, body string) error {
	s.mu.Lock()
	var announce []string
	for _, h := range s.hh {
		if h.LastContract != contractVersion {
			if h.LastContract != "" || len(h.Agents) > 0 { // a brand-new household has no one to tell
				announce = append(announce, h.ID)
			}
			h.LastContract = contractVersion
		}
	}
	err := s.save()
	s.mu.Unlock()
	if err != nil {
		return err
	}
	for _, hid := range announce {
		s.postSystem(hid, "", body)
	}
	return nil
}

func (s *Store) send(from *Agent, to *string, kind, body string, re *int64, attachments []string) (Message, []string, error) {
	valid := false
	for _, k := range Kinds {
		valid = valid || k == kind
	}
	if !valid {
		return Message{}, nil, fmt.Errorf("%w: kind must be one of %s", ErrBadRequest, strings.Join(Kinds, ", "))
	}
	if strings.TrimSpace(body) == "" {
		return Message{}, nil, fmt.Errorf("%w: body is required", ErrBadRequest)
	}
	if len(body) > MaxBody {
		return Message{}, nil, fmt.Errorf("%w: body is capped at 256 KB", ErrBadRequest)
	}
	if to != nil && *to == "" {
		to = nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	h := s.household(from.HouseholdID)
	if h == nil {
		return Message{}, nil, ErrNotFound
	}
	s.expire()
	var rcpts []string
	for _, a := range h.Agents {
		if (to == nil && a.ID != from.ID) || (to != nil && a.ID == *to) {
			rcpts = append(rcpts, a.ID)
		}
	}
	if to != nil && len(rcpts) == 0 {
		return Message{}, nil, fmt.Errorf("%w: unknown to_agent_id; see list_agents", ErrBadRequest)
	}
	if len(rcpts) == 0 {
		return Message{}, nil, fmt.Errorf("%w: there are no other agents in this household yet", ErrBadRequest)
	}
	for _, r := range rcpts {
		if len(s.inbox[r]) >= MaxInbox {
			return Message{}, nil, ErrInboxFull
		}
	}
	atts, err := s.attach(from, attachments)
	if err != nil {
		return Message{}, nil, err
	}
	s.nextID++
	now := time.Now()
	fromID := from.ID
	m := Message{ID: s.nextID, From: &fromID, FromName: from.Name, To: to, Kind: kind, Body: body, Re: re,
		Attachments: atts, CreatedAt: now.UTC().Format(time.RFC3339Nano), created: now}
	for _, r := range rcpts {
		s.inbox[r] = append(s.inbox[r], m)
	}
	s.retain(atts, len(rcpts))
	return m, rcpts, nil
}

// Inbox deletes every message with id <= after (the caller has handled them)
// and returns what is still waiting.
func (s *Store) Inbox(a *Agent, after int64) []Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expire()
	box := s.inbox[a.ID]
	i := 0
	for i < len(box) && box[i].ID <= after {
		i++
	}
	s.release(box[:i])
	box = box[i:]
	if len(box) == 0 {
		delete(s.inbox, a.ID)
		return []Message{}
	}
	s.inbox[a.ID] = box
	return append([]Message(nil), box...)
}

// expire drops messages older than TTL. Caller holds mu.
func (s *Store) expire() {
	cut := time.Now().Add(-TTL)
	for id, box := range s.inbox {
		i := 0
		for i < len(box) && box[i].created.Before(cut) {
			i++
		}
		s.release(box[:i])
		if i == len(box) {
			delete(s.inbox, id)
		} else if i > 0 {
			s.inbox[id] = box[i:]
		}
	}
	s.gcFiles()
}
