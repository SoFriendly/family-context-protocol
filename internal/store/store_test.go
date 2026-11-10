package store

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func setup(t *testing.T) (*Store, string, *Agent, *Agent, *Agent) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fcp-keys.json")
	s, created, err := Open(path)
	if err != nil || !created {
		t.Fatal(err, created)
	}
	hid, _, _ := s.CreateHousehold()
	mint := func(n string) *Agent {
		a, _, err := s.MintAgent(hid, n)
		if err != nil {
			t.Fatal(err)
		}
		return &a
	}
	a, b, c := mint("A"), mint("B"), mint("C")
	s.inbox = map[string][]Message{} // drop the "joined" system notices
	return s, path, a, b, c
}

func TestDirectReplyAndDeleteOnRead(t *testing.T) {
	s, _, a, b, c := setup(t)
	m, to, err := s.Send(a, &b.ID, "task", "add parking", nil)
	if err != nil || len(to) != 1 || to[0] != b.ID {
		t.Fatal(err, to)
	}
	if got := s.Inbox(c, 0); len(got) != 0 {
		t.Fatalf("C sees B's mail: %v", got)
	}
	got := s.Inbox(b, 0)
	if len(got) != 1 || got[0].ID != m.ID || got[0].FromName != "A" {
		t.Fatalf("B inbox: %+v", got)
	}
	// Not acknowledged yet: still there on re-read (a lost response is re-delivered).
	if len(s.Inbox(b, 0)) != 1 {
		t.Fatal("message vanished before ack")
	}
	s.Send(b, &a.ID, "reply", "Added to trip plan", &m.ID)
	if got := s.Inbox(b, m.ID); len(got) != 0 {
		t.Fatalf("ack did not delete: %v", got)
	}
	r := s.Inbox(a, 0)
	if len(r) != 1 || *r[0].Re != m.ID || r[0].Body != "Added to trip plan" {
		t.Fatalf("A inbox: %+v", r)
	}
	s.Inbox(a, r[0].ID)
	if len(s.inbox) != 0 {
		t.Fatalf("server still holds messages: %v", s.inbox)
	}
}

func TestBroadcast(t *testing.T) {
	s, _, a, b, c := setup(t)
	_, to, _ := s.Send(a, nil, "update", "hi all", nil)
	if len(to) != 2 || len(s.Inbox(a, 0)) != 0 || len(s.Inbox(b, 0)) != 1 || len(s.Inbox(c, 0)) != 1 {
		t.Fatalf("broadcast: %v", to)
	}
}

func TestValidation(t *testing.T) {
	s, _, a, _, _ := setup(t)
	bad := "nobody"
	for _, tc := range []struct {
		to         *string
		kind, body string
	}{
		{nil, "gossip", "x"},
		{nil, "task", "  "},
		{&bad, "task", "x"},
		{nil, "task", strings.Repeat("x", MaxBody+1)},
	} {
		if _, _, err := s.Send(a, tc.to, tc.kind, tc.body, nil); !errors.Is(err, ErrBadRequest) {
			t.Errorf("%+v: %v", tc, err)
		}
	}
}

func TestInboxFullAndTTL(t *testing.T) {
	s, _, a, b, _ := setup(t)
	for range MaxInbox {
		if _, _, err := s.Send(a, &b.ID, "notice", "x", nil); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := s.Send(a, &b.ID, "notice", "x", nil); !errors.Is(err, ErrInboxFull) {
		t.Fatalf("full: %v", err)
	}
	for i := range s.inbox[b.ID] {
		s.inbox[b.ID][i].created = time.Now().Add(-TTL - time.Second)
	}
	if got := s.Inbox(b, 0); len(got) != 0 {
		t.Fatalf("expired messages delivered: %d", len(got))
	}
}

func TestRestartKeepsKeysDropsMessages(t *testing.T) {
	s, path, a, b, _ := setup(t)
	_, key, _ := s.MintAgent(a.HouseholdID, "D")
	m, _, _ := s.Send(a, &b.ID, "task", "x", nil)
	fi, _ := os.Stat(path)
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("keys file mode %v", fi.Mode())
	}
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), key) || strings.Contains(string(raw), `"x"`) {
		t.Fatal("keys file holds a raw key or message content")
	}
	s2, created, err := Open(path)
	if err != nil || created {
		t.Fatal(err, created)
	}
	d, err := s2.AgentByKey(key)
	if err != nil || d.Name != "D" || d.HouseholdID != a.HouseholdID {
		t.Fatal("key lost across restart", err)
	}
	if got := s2.Inbox(b, 0); len(got) != 0 {
		t.Fatal("message survived restart")
	}
	m2, _, _ := s2.Send(a, &b.ID, "task", "y", nil)
	if m2.ID <= m.ID {
		t.Fatalf("ids repeat across restart: %d <= %d", m2.ID, m.ID)
	}
}

func TestRotateRevoke(t *testing.T) {
	s, _, a, b, _ := setup(t)
	_, oldKey, _ := s.MintAgent(a.HouseholdID, "E")
	e, _ := s.AgentByKey(oldKey)
	newKey, _ := s.RotateAgent(a.HouseholdID, e.ID)
	if _, err := s.AgentByKey(oldKey); err == nil {
		t.Fatal("old key works")
	}
	if _, err := s.AgentByKey(newKey); err != nil {
		t.Fatal("new key fails")
	}
	s.Send(a, &b.ID, "notice", "x", nil)
	s.DeleteAgent(a.HouseholdID, b.ID)
	if _, ok := s.inbox[b.ID]; ok {
		t.Fatal("revoked agent's inbox kept")
	}
}

func TestConcurrentIDsUnique(t *testing.T) {
	s, _, a, b, _ := setup(t)
	var wg sync.WaitGroup
	for range 200 {
		wg.Go(func() { s.Send(a, &b.ID, "notice", "x", nil) })
	}
	wg.Wait()
	box := s.Inbox(b, 0)
	for i := 1; i < len(box); i++ {
		if box[i].ID <= box[i-1].ID {
			t.Fatal("ids not strictly increasing in inbox order")
		}
	}
	if len(box) != 200 {
		t.Fatalf("got %d", len(box))
	}
}

func TestInvites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "k.json")
	s, _, _ := Open(path)
	hid, _, _ := s.CreateHousehold()
	if got := s.EmptyHouseholds(); len(got) != 1 || got[0] != hid {
		t.Fatalf("empty: %v", got)
	}
	tok, _, err := s.CreateInvite(hid)
	if err != nil || s.CheckInvite(tok) != nil {
		t.Fatal(err)
	}
	red, err := s.Redeem(tok, "Jamie's Muse")
	a, key := red.Agent, red.Key
	if err != nil || red.PartnerToken == "" || a.Name != "Jamie's Muse" {
		t.Fatal(err, red)
	}
	if got, _ := s.AgentByKey(key); got == nil || got.HouseholdID != hid {
		t.Fatal("redeemed key does not work")
	}
	// A retry within the window (lost response) gets the same agent, a new key,
	// and the same partner link; the old key stops working.
	retry, err := s.Redeem(tok, "again")
	if err != nil || retry.Agent.ID != a.ID || retry.Key == key || retry.PartnerToken != red.PartnerToken {
		t.Fatalf("retry: %v %+v", err, retry)
	}
	if _, err := s.AgentByKey(key); err == nil {
		t.Fatal("pre-retry key still works")
	}
	s.recent[Hash(tok)] = recentJoin{until: time.Now().Add(-time.Second)}
	if _, err := s.Redeem(tok, "late"); !errors.Is(err, ErrGone) {
		t.Fatalf("reuse after window: %v", err)
	}
	tok2, _, _ := s.CreateInvite(hid)
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), tok2) {
		t.Fatal("raw invite token on disk")
	}
	s2, _, _ := Open(path) // invites survive restart
	red2, err := s2.Redeem(tok2, "")
	if err != nil || red2.PartnerToken != "" || red2.Agent.Name != "Agent 2" {
		t.Fatal(err, red2)
	}
	tok3, _, _ := s2.CreateInvite(hid)
	inv := s2.hh[0].Invites
	inv[len(inv)-1].Expires = time.Now().Add(-time.Second)
	if s2.CheckInvite(tok3) == nil {
		t.Fatal("expired invite accepted")
	}
}

func TestKeyLink(t *testing.T) {
	s, _, a, _, _ := setup(t)
	_, oldKey, _ := s.MintAgent(a.HouseholdID, "Alex's Muse")
	ag, _ := s.AgentByKey(oldKey)
	tok := s.CreateKeyLink(ag.HouseholdID, ag.ID)
	if name, err := s.KeyLinkAgent(tok); err != nil || name != "Alex's Muse" {
		t.Fatal(name, err)
	}
	name, key, err := s.RevealKey(tok)
	if err != nil || name != "Alex's Muse" || key == oldKey {
		t.Fatal(name, err)
	}
	if got, _ := s.AgentByKey(key); got == nil || got.ID != ag.ID {
		t.Fatal("revealed key doesn't work")
	}
	if _, err := s.AgentByKey(oldKey); err == nil {
		t.Fatal("old key still works")
	}
	if _, _, err := s.RevealKey(tok); !errors.Is(err, ErrGone) {
		t.Fatal("key link reusable")
	}
}

func TestSetUpAgent(t *testing.T) {
	s, _, _ := Open(filepath.Join(t.TempDir(), "k.json"))
	hid, _, _ := s.CreateHousehold()
	a, key, tok, _, err := s.SetUpAgent(hid, "Jamie's Muse")
	if err != nil {
		t.Fatal(err)
	}
	red, err := s.Redeem(tok, "ignored")
	if err != nil || red.Agent.ID != a.ID || red.Key != "" || red.PartnerToken == "" {
		t.Fatalf("bound redeem: %v %+v", err, red)
	}
	if got, _ := s.AgentByKey(key); got == nil || got.ID != a.ID {
		t.Fatal("admin-shown key stopped working")
	}
	if retry, err := s.Redeem(tok, ""); err != nil || retry.Key != "" || retry.Agent.ID != a.ID {
		t.Fatalf("bound retry must not rotate: %v %+v", err, retry)
	}
	if _, err := s.AgentByKey(key); err != nil {
		t.Fatal("retry rotated the key")
	}
	if as, _ := s.ListAgents(hid); len(as) != 1 {
		t.Fatalf("agents: %d", len(as))
	}
	// Revoked before redeeming: the link is dead.
	b, _, tok2, _, _ := s.SetUpAgent(hid, "B")
	s.DeleteAgent(hid, b.ID)
	if _, err := s.Redeem(tok2, ""); !errors.Is(err, ErrGone) {
		t.Fatalf("revoked bound invite: %v", err)
	}
}

func TestSystemMessages(t *testing.T) {
	s, _, _ := Open(filepath.Join(t.TempDir(), "k.json"))
	hid, _, _ := s.CreateHousehold()
	if err := s.AnnounceRelease("v1", "first"); err != nil { // brand-new, empty household: no one to tell
		t.Fatal(err)
	}
	j, _, _ := s.MintAgent(hid, "Jamie's Muse")
	if got := s.Inbox(&j, 0); len(got) != 0 {
		t.Fatalf("first agent got %v", got)
	}
	a, _, _ := s.MintAgent(hid, "Alex's Grok")
	got := s.Inbox(&j, 0)
	if len(got) != 1 || got[0].Kind != SystemKind || got[0].From != nil || got[0].FromName != "FCP" || !strings.Contains(got[0].Body, "Alex's Grok joined") {
		t.Fatalf("join notice: %+v", got)
	}
	if len(s.Inbox(&a, 0)) != 0 {
		t.Fatal("new agent was told about itself")
	}
	s.Inbox(&j, got[0].ID)
	// A release announces once per contract change, to everyone.
	s.AnnounceRelease("v1", "same")
	if len(s.Inbox(&j, 0)) != 0 {
		t.Fatal("unchanged contract announced")
	}
	s.AnnounceRelease("v2", "FCP was updated")
	if g := s.Inbox(&j, 0); len(g) != 1 || g[0].Body != "FCP was updated" || len(s.Inbox(&a, 0)) != 1 {
		t.Fatalf("release notice: %+v", g)
	}
	s2, _, _ := Open(s.path) // remembered across restarts
	s2.AnnounceRelease("v2", "again")
	if len(s2.inbox) != 0 {
		t.Fatal("re-announced after restart")
	}
	// Removal is announced to the rest.
	s.inbox = map[string][]Message{}
	s.DeleteAgent(hid, a.ID)
	if g := s.Inbox(&j, 0); len(g) != 1 || !strings.Contains(g[0].Body, "Alex's Grok was removed") {
		t.Fatalf("removal notice: %+v", g)
	}
	// Agents can't send system messages.
	if _, _, err := s.Send(&j, nil, SystemKind, "x", nil); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("agent sent system: %v", err)
	}
}

func TestAttachments(t *testing.T) {
	s, _, a, b, c := setup(t)
	// Two-step upload: declare, then put exactly the declared bytes once.
	id, tok, err := s.NewUpload(a, "../../etc/plan.pdf", "application/pdf", 5)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutUpload(tok, []byte("1234")); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("short upload: %v", err)
	}
	att, err := s.PutUpload(tok, []byte("12345"))
	if err != nil || att.Name != "plan.pdf" || att.DownloadURL != "/v1/files/"+id {
		t.Fatalf("put: %v %+v", err, att)
	}
	if _, err := s.PutUpload(tok, []byte("12345")); !errors.Is(err, ErrGone) {
		t.Fatal("upload token reusable")
	}
	// Only the uploader can attach it.
	if _, _, err := s.Send(b, &c.ID, "notice", "x", nil, id); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("someone else's file: %v", err)
	}
	// Broadcast: freed only after every recipient acknowledges.
	m, _, err := s.Send(a, nil, "update", "the plan", nil, id)
	if err != nil || len(m.Attachments) != 1 {
		t.Fatal(err, m)
	}
	if infos, _ := s.ListAgents(a.HouseholdID); infos[1].PendingFiles != 1 || infos[1].PendingFileBytes != 5 {
		t.Fatalf("admin counts: %+v", infos)
	}
	s.Inbox(b, m.ID)
	if _, data, err := s.File(id); err != nil || string(data) != "12345" {
		t.Fatal("freed before the last recipient acked")
	}
	s.Inbox(c, m.ID)
	if _, _, err := s.File(id); !errors.Is(err, ErrNotFound) || s.fileBytes != 0 {
		t.Fatalf("not freed after all acks: %v bytes=%d", err, s.fileBytes)
	}
	// Caps and cleanup.
	if _, err := s.AddFile(a, "big", "", make([]byte, MaxFile+1)); !errors.Is(err, ErrTooBig) {
		t.Fatalf("too big: %v", err)
	}
	for range MaxFilesTotal / MaxFile {
		if _, _, err := s.NewUpload(a, "f", "", MaxFile); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := s.NewUpload(a, "f", "", 1); !errors.Is(err, ErrStorageFull) {
		t.Fatalf("total cap: %v", err)
	}
	for _, f := range s.files {
		f.uploadUntil = time.Now().Add(-time.Second)
	}
	s.mu.Lock()
	s.gcFiles()
	s.mu.Unlock()
	if len(s.files) != 0 || s.fileBytes != 0 {
		t.Fatalf("abandoned uploads kept: %d files, %d bytes", len(s.files), s.fileBytes)
	}
	unsent, _ := s.AddFile(a, "unsent.txt", "", []byte("x"))
	s.files[unsent.ID].created = time.Now().Add(-2 * time.Hour)
	if _, _, err := s.File(unsent.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("never-sent file kept past its hour")
	}
}
