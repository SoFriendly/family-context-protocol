package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/sofriendly/family-context-protocol/internal/store"
)

type env struct {
	t   *testing.T
	url string
	hh  string // household key
}

func setup(t *testing.T) *env {
	t.Helper()
	st, _, err := store.Open(filepath.Join(t.TempDir(), "fcp-keys.json"))
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(New(st, Config{}))
	t.Cleanup(ts.Close)
	_, hh, _ := st.CreateHousehold()
	return &env{t: t, url: ts.URL, hh: hh}
}

// do sends a JSON request and decodes the response, failing on an unexpected status.
func (e *env) do(method, path, key string, body any, want int, out any) map[string]any {
	e.t.Helper()
	var rd io.Reader
	if s, ok := body.(string); ok {
		rd = strings.NewReader(s)
	} else if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, e.url+path, rd)
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != want {
		e.t.Fatalf("%s %s: status %d, want %d: %s", method, path, resp.StatusCode, want, raw)
	}
	var m map[string]any
	json.Unmarshal(raw, &m)
	if out != nil {
		json.Unmarshal(raw, out)
	}
	return m
}

type minted struct {
	Key string `json:"agent_key"`
	ID  string `json:"agent_id"`
}

func (e *env) mint(name string) minted {
	var m minted
	e.do("POST", "/v1/agents", e.hh, map[string]string{"name": name}, 201, &m)
	return m
}

type inbox struct{ Messages []store.Message }

// inbox returns the agent-to-agent messages waiting for key (system notices
// such as "X joined" are left out; TestSystemNotices covers them).
func (e *env) inbox(key string, after int64) []store.Message {
	var in inbox
	e.do("GET", fmt.Sprintf("/v1/inbox?after=%d", after), key, nil, 200, &in)
	return fromAgents(in.Messages)
}

func fromAgents(msgs []store.Message) []store.Message {
	out := []store.Message{}
	for _, m := range msgs {
		if m.Kind != store.SystemKind {
			out = append(out, m)
		}
	}
	return out
}

// TestCoordinationREST is the worked example over plain REST: update + task,
// the other side picks it up and replies, the sender sees the reply, and once
// both have acknowledged, the server holds nothing.
func TestCoordinationREST(t *testing.T) {
	e := setup(t)
	j, a, c := e.mint("Jamie's Muse"), e.mint("Alex's Muse"), e.mint("Third")

	var agents struct {
		Agents []store.AgentInfo
		You    string
	}
	e.do("GET", "/v1/agents", j.Key, nil, 200, &agents)
	if len(agents.Agents) != 3 || agents.You != j.ID {
		t.Fatalf("agents: %+v", agents)
	}

	// Leaving out the recipient is an error, never a silent broadcast.
	if m := e.do("POST", "/v1/messages", j.Key, map[string]any{"kind": "update", "body": "Booked MCO parking"}, 400, nil); !strings.Contains(m["hint"].(string), "/v1/broadcast") {
		t.Fatalf("missing recipient hint: %v", m)
	}
	e.do("POST", "/v1/broadcast", j.Key, map[string]any{"to_agent_id": a.ID, "kind": "update", "body": "x"}, 400, nil)
	e.do("POST", "/v1/broadcast", j.Key, map[string]any{"kind": "update", "body": "Booked MCO parking"}, 201, nil) // to everyone
	var task struct {
		ID          int64
		DeliveredTo []string `json:"delivered_to"`
	}
	e.do("POST", "/v1/messages", j.Key, map[string]any{"to_agent_id": a.ID, "kind": "task",
		"body": "Add MCO parking (Garage C, $264, entry from 1:30 PM Sat) to the trip plan"}, 201, &task)
	if len(task.DeliveredTo) != 1 || task.DeliveredTo[0] != a.ID {
		t.Fatalf("task: %+v", task)
	}

	if got := e.inbox(c.Key, 0); len(got) != 1 || got[0].Kind != "update" {
		t.Fatalf("third agent should see only the broadcast: %+v", got)
	}
	got := e.inbox(a.Key, 0)
	if len(got) != 2 || got[1].ID != task.ID || got[1].FromName != "Jamie's Muse" {
		t.Fatalf("Alex's inbox: %+v", got)
	}
	e.do("POST", "/v1/messages", a.Key, map[string]any{"to_agent_id": j.ID, "kind": "reply", "re": task.ID, "body": "Added to trip plan"}, 201, nil)
	if left := e.inbox(a.Key, task.ID); len(left) != 0 {
		t.Fatalf("ack left messages: %+v", left)
	}

	r := e.inbox(j.Key, 0)
	if len(r) != 1 || r[0].Kind != "reply" || *r[0].Re != task.ID || r[0].Body != "Added to trip plan" {
		t.Fatalf("Jamie's inbox: %+v", r)
	}
	e.inbox(j.Key, r[0].ID)
	e.inbox(c.Key, 1<<62)

	e.do("GET", "/v1/agents", e.hh, nil, 200, &agents)
	for _, ag := range agents.Agents {
		if ag.Pending != 0 {
			t.Fatalf("server still holds messages: %+v", agents.Agents)
		}
	}

	// Rotate: old key 401s, new key works.
	var rot minted
	e.do("POST", "/v1/agents/"+j.ID+"/rotate", e.hh, nil, 200, &rot)
	if m := e.do("GET", "/v1/inbox", j.Key, nil, 401, nil); m["hint"] == nil {
		t.Fatal("old key: no hint")
	}
	e.do("GET", "/v1/inbox", rot.Key, nil, 200, nil)
	// Revoke.
	e.do("DELETE", "/v1/agents/"+c.ID, e.hh, nil, 200, nil)
	e.do("GET", "/v1/inbox", c.Key, nil, 401, nil)
	e.do("POST", "/v1/messages", rot.Key, map[string]any{"to_agent_id": c.ID, "kind": "notice", "body": "x"}, 400, nil)
}

func TestValidation(t *testing.T) {
	e := setup(t)
	a := e.mint("A")
	e.mint("B")
	e.do("POST", "/v1/messages", a.Key, map[string]any{"kind": "gossip", "body": "x"}, 400, nil)
	e.do("POST", "/v1/messages", a.Key, map[string]any{"kind": "task"}, 400, nil)
	e.do("POST", "/v1/messages", a.Key, "{nope", 400, nil)
	e.do("GET", "/v1/inbox?after=x", a.Key, nil, 400, nil)
	big, _ := json.Marshal(map[string]string{"kind": "task", "body": strings.Repeat("x", 400<<10)})
	if m := e.do("POST", "/v1/messages", a.Key, string(big), 413, nil); m["error"] != "payload too large" {
		t.Fatalf("413: %v", m)
	}
}

// TestCoordinationMCP runs the same loop through the MCP tools.
func TestCoordinationMCP(t *testing.T) {
	e := setup(t)
	j, a := e.mint("Jamie's Muse"), e.mint("Alex's Muse")
	var list struct {
		Agents []store.AgentInfo
		You    string
	}
	e.mcp(j.Key, "list_agents", map[string]any{}, &list)
	if len(list.Agents) != 2 || list.You != j.ID {
		t.Fatalf("list_agents: %+v", list)
	}
	var sent struct{ ID int64 }
	e.mcp(j.Key, "send_message", map[string]any{"to_agent_id": a.ID, "kind": "task", "body": "Add MCO parking to the trip plan"}, &sent)
	var in inbox
	e.mcp(a.Key, "check_inbox", map[string]any{}, &in)
	in.Messages = fromAgents(in.Messages)
	if len(in.Messages) != 1 || in.Messages[0].ID != sent.ID {
		t.Fatalf("check_inbox: %+v", in)
	}
	e.mcp(a.Key, "send_message", map[string]any{"to_agent_id": j.ID, "kind": "reply", "re": sent.ID, "body": "Added to trip plan"}, nil)
	e.mcp(a.Key, "check_inbox", map[string]any{"after": sent.ID}, &in)
	if len(in.Messages) != 0 {
		t.Fatalf("ack: %+v", in)
	}
	e.mcp(j.Key, "check_inbox", map[string]any{}, &in)
	in.Messages = fromAgents(in.Messages)
	if len(in.Messages) != 1 || in.Messages[0].Body != "Added to trip plan" {
		t.Fatalf("reply: %+v", in)
	}
	if isErr, text := e.mcp(j.Key, "send_message", map[string]any{"to_agent_id": a.ID, "kind": "gossip", "body": "x"}, nil); !isErr || !strings.Contains(text, "kind must be") {
		t.Fatalf("bad kind: %v %s", isErr, text)
	}
	// No recipient is an error; broadcasting is its own tool.
	for _, args := range []map[string]any{{"kind": "notice", "body": "x"}, {"to_agent_id": "", "kind": "notice", "body": "x"}} {
		if isErr, text := e.mcp(j.Key, "send_message", args, nil); !isErr {
			t.Fatalf("send_message without recipient: %s", text)
		}
	}
	var b struct {
		To []string `json:"delivered_to"`
	}
	if isErr, text := e.mcp(j.Key, "broadcast_message", map[string]any{"kind": "notice", "body": "hi all"}, &b); isErr || len(b.To) != 1 || b.To[0] != a.ID {
		t.Fatalf("broadcast_message: %s %+v", text, b)
	}
}

func TestAuth(t *testing.T) {
	e := setup(t)
	a := e.mint("A")
	cases := []struct {
		method, path, key string
		want              int
	}{
		{"GET", "/v1/inbox", "", 401},
		{"GET", "/v1/inbox", "fcpa_nope", 401},
		{"GET", "/v1/inbox", "fcph_nope", 401},
		{"GET", "/v1/inbox", e.hh, 401},        // household key can't read anyone's inbox
		{"POST", "/v1/messages", e.hh, 401},    // or send as an agent
		{"POST", "/v1/agents", a.Key, 401},     // agent key on admin route
		{"DELETE", "/v1/agents/x", a.Key, 401}, // agent key on admin route
		{"POST", "/mcp", e.hh, 401},            // household key on MCP
		{"POST", "/mcp", "fcpa_nope", 401},     // unknown key on MCP
		{"POST", "/v1/agents/nope/rotate", e.hh, 404},
		{"GET", "/v1/nope", a.Key, 404},
	}
	for _, c := range cases {
		m := e.do(c.method, c.path, c.key, map[string]string{"name": "x"}, c.want, nil)
		if m["error"] == nil || m["hint"] == nil {
			t.Errorf("%s %s: missing error/hint: %v", c.method, c.path, m)
		}
	}
	if m := e.do("POST", "/mcp", e.hh, "{}", 401, nil); !strings.Contains(m["hint"].(string), "fcpa_") {
		t.Errorf("mcp household-key hint: %v", m)
	}
}

func TestAdminTokenAndCreateLimit(t *testing.T) {
	st, _, _ := store.Open(filepath.Join(t.TempDir(), "fcp-keys.json"))
	ts := httptest.NewServer(New(st, Config{AdminToken: "s3cret"}))
	defer ts.Close()
	e := &env{t: t, url: ts.URL}
	off := httptest.NewServer(New(st, Config{}))
	defer off.Close()
	if m := (&env{t: t, url: off.URL}).do("POST", "/v1/households", "", nil, 403, nil); m["error"] != "household creation disabled" {
		t.Fatalf("no admin token: %v", m)
	}
	e.do("POST", "/v1/households", "", nil, 401, nil)
	e.do("POST", "/v1/households", "wrong", nil, 401, nil)
	for range 5 {
		e.do("POST", "/v1/households", "s3cret", nil, 201, nil)
	}
	if m := e.do("POST", "/v1/households", "s3cret", nil, 429, nil); m["error"] != "rate limited" {
		t.Fatalf("6th create: %v", m)
	}
}

func TestLimiter(t *testing.T) {
	l := NewLimiter()
	for i := range 30 {
		if !l.Allow("k", true) {
			t.Fatalf("write %d refused", i)
		}
	}
	if l.Allow("k", true) {
		t.Fatal("31st write allowed")
	}
	if !l.Allow("k", false) || !l.Allow("other", true) {
		t.Fatal("reads and other keys have their own buckets")
	}
	b := l.b["w:"+store.Hash("k")]
	b.last = b.last.Add(-60e9)
	if !l.Allow("k", true) {
		t.Fatal("bucket did not refill")
	}
}

func TestAdminUI(t *testing.T) {
	e := setup(t)
	c := &http.Client{Jar: &cookieJar{}}
	resp, _ := c.PostForm(e.url+"/admin/login", map[string][]string{"key": {e.hh}})
	resp.Body.Close()
	resp, _ = c.PostForm(e.url+"/admin/setup", map[string][]string{"name": {"Jamie's Muse"}})
	page, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	key := regexp.MustCompile(`fcpa_[0-9a-f]{64}`).FindString(string(page))
	link := regexp.MustCompile(`http://[^\s<]+/join/fcpi_[0-9a-f]+`).FindString(string(page))
	if key == "" || link == "" || !strings.Contains(string(page), "Please connect to our family") {
		t.Fatalf("setup page lacks key or message: %s", page)
	}
	// The assistant redeems the message's link: same agent, no new key.
	var j map[string]any
	e.do("POST", strings.TrimPrefix(link, e.url), "", map[string]string{"name": "x"}, 201, &j)
	if j["agent_key"] != nil || j["agent_key_from_user"] != true || j["name"] != "Jamie's Muse" {
		t.Fatalf("bound join: %v", j)
	}
	e.do("GET", "/v1/inbox", key, nil, 200, nil)
	resp, _ = c.Get(e.url + "/admin")
	page, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if strings.Contains(string(page), "fcpa_") || strings.Count(string(page), "Jamie&#39;s Muse") != 1 {
		t.Fatalf("admin page: %s", page)
	}
}

// cookieJar is a single-host jar, enough for the test server.
type cookieJar struct{ cs []*http.Cookie }

func (j *cookieJar) SetCookies(_ *url.URL, cs []*http.Cookie) { j.cs = append(j.cs, cs...) }
func (j *cookieJar) Cookies(*url.URL) []*http.Cookie          { return j.cs }

// mcp calls a tool over the MCP endpoint and decodes its structured result.
func (e *env) mcp(key, tool string, args any, out any) (isError bool, text string) {
	e.t.Helper()
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": tool, "arguments": args}})
	req, _ := http.NewRequest("POST", e.url+"/mcp", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	var rpc struct {
		Result struct {
			IsError           bool `json:"isError"`
			Content           []struct{ Text string }
			StructuredContent json.RawMessage `json:"structuredContent"`
		}
		Error any
	}
	raw, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(raw, &rpc); err != nil || rpc.Error != nil {
		e.t.Fatalf("mcp %s: %s", tool, raw)
	}
	if out != nil {
		json.Unmarshal(rpc.Result.StructuredContent, out)
	}
	if len(rpc.Result.Content) > 0 {
		text = rpc.Result.Content[0].Text
	}
	return rpc.Result.IsError, text
}

// TestJoinFlow: paste a link into an agent, it joins and gets a partner link;
// the partner's agent joins with that; then they can message each other.
func TestJoinFlow(t *testing.T) {
	e := setup(t)
	var inv struct {
		Link string `json:"invite_link"`
	}
	e.do("POST", "/v1/invites", e.hh, nil, 201, &inv)
	if !strings.HasPrefix(inv.Link, e.url+"/join/fcpi_") {
		t.Fatalf("invite link: %s", inv.Link)
	}
	path := strings.TrimPrefix(inv.Link, e.url)

	get := func(p string) (int, string) {
		resp, err := http.Get(e.url + p)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	// GET (link previews, the agent reading it) shows instructions, twice, without using it up.
	for range 2 {
		code, body := get(path)
		if code != 200 || !strings.Contains(body, "POST "+e.url+path) || !strings.Contains(body, "check_inbox") {
			t.Fatalf("GET invite: %d %s", code, body)
		}
	}

	var j1 struct {
		Key     string `json:"agent_key"`
		MCP     string `json:"mcp_url"`
		Partner string `json:"partner_link"`
		Message string `json:"partner_message"`
	}
	e.do("POST", path, "", map[string]string{"name": "Jamie's Muse"}, 201, &j1)
	if !strings.HasSuffix(j1.Message, "\n"+j1.Partner) || !strings.Contains(j1.Message, "coordinate with Jamie's Muse") {
		t.Fatalf("partner message: %q", j1.Message)
	}
	if j1.MCP != e.url+"/mcp" || !strings.HasPrefix(j1.Partner, e.url+"/join/fcpi_") {
		t.Fatalf("first join: %+v", j1)
	}
	// Retry (as after a timeout): same agent, new key, same partner link.
	var retry struct {
		ID      string `json:"agent_id"`
		Key     string `json:"agent_key"`
		Partner string `json:"partner_link"`
	}
	e.do("POST", path, "", map[string]string{"name": "again"}, 201, &retry)
	if retry.Key == j1.Key || retry.Partner != j1.Partner {
		t.Fatalf("retry: %+v", retry)
	}
	e.do("GET", "/v1/inbox", j1.Key, nil, 401, nil)
	j1.Key = retry.Key
	if code, _ := get(path); code != 410 {
		t.Fatalf("used invite GET: %d", code)
	}

	var j2 map[string]any
	e.do("POST", strings.TrimPrefix(j1.Partner, e.url), "", map[string]string{"name": "Alex's Muse"}, 201, &j2)
	if _, ok := j2["partner_link"]; ok {
		t.Fatal("second agent should not get a partner link")
	}
	// They're in the same household and can talk.
	e.do("POST", "/v1/broadcast", j2["agent_key"].(string), map[string]any{"kind": "notice", "body": "hi from Alex's Muse"}, 201, nil)
	if got := e.inbox(j1.Key, 0); len(got) != 1 || got[0].FromName != "Alex's Muse" {
		t.Fatalf("Jamie's inbox: %+v", got)
	}
	if code, body := get("/agents.md"); code != 200 || !strings.Contains(body, "check_inbox") {
		t.Fatalf("agents.md: %d", code)
	}
}

func TestContractVersion(t *testing.T) {
	e := setup(t)
	a := e.mint("A")
	if len(ContractVersion) != 8 {
		t.Fatalf("version %q", ContractVersion)
	}
	if m := e.do("GET", "/v1/inbox", a.Key, nil, 200, nil); m["contract_version"] != ContractVersion {
		t.Fatalf("inbox: %v", m)
	}
	if m := e.do("GET", "/v1/agents", a.Key, nil, 200, nil); m["contract_version"] != ContractVersion {
		t.Fatalf("agents: %v", m)
	}
	var out struct {
		V string `json:"contract_version"`
	}
	e.mcp(a.Key, "check_inbox", map[string]any{}, &out)
	if out.V != ContractVersion {
		t.Fatalf("mcp check_inbox: %+v", out)
	}
}

// TestKeyLink: for assistants that can't store a key from a response, the
// person opens key_link, presses the button, and gets a fresh key once.
func TestKeyLink(t *testing.T) {
	e := setup(t)
	var inv struct {
		Link string `json:"invite_link"`
	}
	e.do("POST", "/v1/invites", e.hh, nil, 201, &inv)
	var j struct {
		Key     string `json:"agent_key"`
		KeyLink string `json:"key_link"`
	}
	e.do("POST", strings.TrimPrefix(inv.Link, e.url), "", map[string]string{"name": "Alex's Muse"}, 201, &j)
	if !strings.HasPrefix(j.KeyLink, e.url+"/key/fcpk_") {
		t.Fatalf("key_link: %q", j.KeyLink)
	}
	// GET (a link preview, or the person) only shows a button.
	for range 2 {
		resp, _ := http.Get(j.KeyLink)
		page, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || strings.Contains(string(page), "fcpa_") || !strings.Contains(string(page), "Show key") {
			t.Fatalf("GET key link: %d %s", resp.StatusCode, page)
		}
	}
	resp, _ := http.PostForm(j.KeyLink, nil)
	page, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	i := strings.Index(string(page), "fcpa_")
	if resp.StatusCode != 200 || i < 0 {
		t.Fatalf("reveal: %d %s", resp.StatusCode, page)
	}
	newKey := string(page[i : i+5+64])
	e.do("GET", "/v1/inbox", newKey, nil, 200, nil)
	e.do("GET", "/v1/inbox", j.Key, nil, 401, nil) // the key the assistant saw is dead
	if resp, _ := http.PostForm(j.KeyLink, nil); resp.StatusCode != 410 {
		t.Fatalf("second reveal: %d", resp.StatusCode)
	}

	// Manual key entry: the response never carries a key, only the link.
	e.do("POST", "/v1/invites", e.hh, nil, 201, &inv)
	path := strings.TrimPrefix(inv.Link, e.url)
	e.do("POST", path, "", map[string]string{"name": "x", "key_entry": "chat"}, 400, nil) // doesn't use the invite
	m := e.do("POST", path, "", map[string]string{"name": "Jamie's Duet", "key_entry": "manual"}, 201, nil)
	if _, ok := m["agent_key"]; ok || !strings.HasPrefix(m["key_link"].(string), e.url+"/key/fcpk_") {
		t.Fatalf("manual join: %v", m)
	}
	resp, _ = http.PostForm(m["key_link"].(string), nil)
	page, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	i = strings.Index(string(page), "fcpa_")
	if i < 0 {
		t.Fatalf("manual reveal: %s", page)
	}
	e.do("GET", "/v1/inbox", string(page[i:i+5+64]), nil, 200, nil)
}

// TestPushOptions: a webhook fires the moment a message is sent, and a watch
// URL (no key) reports a count without content.
func TestPushOptions(t *testing.T) {
	st, _, _ := store.Open(filepath.Join(t.TempDir(), "k.json"))
	ts := httptest.NewServer(New(st, Config{AllowPrivateWebhooks: true}))
	defer ts.Close()
	_, hh, _ := st.CreateHousehold()
	e := &env{t: t, url: ts.URL, hh: hh}
	j, a := e.mint("Jamie's Muse"), e.mint("Alex's Grok")

	hooks := make(chan []byte, 1)
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		hooks <- b
	}))
	defer hook.Close()
	var wh struct {
		Secret string `json:"secret"`
	}
	e.do("POST", "/v1/webhook", a.Key, map[string]string{"url": hook.URL}, 200, &wh)
	if !strings.HasPrefix(wh.Secret, "fcps_") {
		t.Fatalf("secret: %q", wh.Secret)
	}
	var watch struct {
		URL string `json:"watch_url"`
	}
	e.do("POST", "/v1/watch", a.Key, nil, 200, &watch)
	peek := func() map[string]any {
		resp, err := http.Get(watch.URL)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var m map[string]any
		json.NewDecoder(resp.Body).Decode(&m)
		return m
	}
	if m := peek(); m["waiting"] != float64(0) {
		t.Fatalf("watch before: %v", m)
	}

	var sent struct{ ID int64 }
	e.do("POST", "/v1/messages", j.Key, map[string]any{"to_agent_id": a.ID, "kind": "task", "body": "Add MCO parking"}, 201, &sent)
	select {
	case b := <-hooks:
		if !strings.Contains(string(b), "Add MCO parking") {
			t.Fatalf("webhook body: %s", b)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("webhook not called")
	}
	m := peek()
	if m["waiting"] != float64(1) || m["newest_id"] != float64(sent.ID) {
		t.Fatalf("watch after: %v", m)
	}
	raw, _ := json.Marshal(m)
	if strings.Contains(string(raw), "parking") {
		t.Fatal("watch URL leaked content")
	}
	e.inbox(a.Key, sent.ID) // ack
	if m := peek(); m["waiting"] != float64(0) {
		t.Fatalf("watch after ack: %v", m)
	}
	// Default config refuses private webhook targets; a new watch URL kills the old one.
	e2 := setup(t)
	b := e2.mint("B")
	e2.do("POST", "/v1/webhook", b.Key, map[string]string{"url": "https://192.168.1.5/hook"}, 400, nil)
	e.do("POST", "/v1/watch", a.Key, nil, 200, nil)
	if resp, _ := http.Get(watch.URL); resp.StatusCode != 404 {
		t.Fatalf("old watch url: %d", resp.StatusCode)
	}
}

// TestMCPBehindProxy: behind Tailscale Funnel or a reverse proxy, FCP listens
// on loopback but requests carry the public Host header. MCP must still work.
func TestMCPBehindProxy(t *testing.T) {
	e := setup(t)
	a := e.mint("A")
	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`
	req, _ := http.NewRequest("POST", e.url+"/mcp", strings.NewReader(body))
	req.Host = "fcp.example.net"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+a.Key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(b), "protocolVersion") {
		t.Fatalf("MCP via public Host: %d %s", resp.StatusCode, b)
	}
	// Without a key, a cross-site request still gets nowhere.
	req2, _ := http.NewRequest("POST", e.url+"/mcp", strings.NewReader(body))
	req2.Host = "evil.example"
	if resp, _ := http.DefaultClient.Do(req2); resp.StatusCode != 401 {
		t.Fatalf("no key: %d", resp.StatusCode)
	}
}

// TestAttachments: upload (direct, and declare + key-free PUT), attach, receive
// a key-free download URL, and the file is gone once the recipient acks.
func TestAttachments(t *testing.T) {
	e := setup(t)
	j, a := e.mint("Jamie's Muse"), e.mint("Alex's Grok")

	req, _ := http.NewRequest("POST", e.url+"/v1/files?name=parking.pdf", strings.NewReader("%PDF-receipt"))
	req.Header.Set("Authorization", "Bearer "+j.Key)
	req.Header.Set("Content-Type", "application/pdf")
	resp, _ := http.DefaultClient.Do(req)
	var direct struct {
		ID string `json:"file_id"`
	}
	json.NewDecoder(resp.Body).Decode(&direct)
	resp.Body.Close()
	if resp.StatusCode != 201 || direct.ID == "" {
		t.Fatalf("direct upload: %d", resp.StatusCode)
	}

	var up struct {
		ID  string `json:"file_id"`
		URL string `json:"upload_url"`
	}
	e.do("POST", "/v1/uploads", j.Key, map[string]any{"name": "map.png", "content_type": "image/png", "size": 4}, 201, &up)
	put := func(body string) int {
		req, _ := http.NewRequest("PUT", up.URL, strings.NewReader(body))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := put("toolong"); code != 413 {
		t.Fatalf("oversized PUT: %d", code)
	}
	if code := put("\x89PNG"); code != 200 {
		t.Fatalf("PUT: %d", code)
	}
	if code := put("\x89PNG"); code != 410 {
		t.Fatalf("second PUT: %d", code)
	}

	var sent struct{ ID int64 }
	e.do("POST", "/v1/messages", j.Key, map[string]any{"to_agent_id": a.ID, "kind": "update", "body": "Parking receipt and map",
		"attachments": []string{direct.ID, up.ID}}, 201, &sent)
	e.do("POST", "/v1/messages", a.Key, map[string]any{"kind": "update", "body": "x", "attachments": []string{direct.ID}}, 400, nil) // not Alex's file

	msgs := e.inbox(a.Key, 0)
	if len(msgs) != 1 || len(msgs[0].Attachments) != 2 {
		t.Fatalf("inbox: %+v", msgs)
	}
	att := msgs[0].Attachments[0]
	if att.Name != "parking.pdf" || att.ContentType != "application/pdf" || !strings.HasPrefix(att.DownloadURL, e.url+"/v1/files/fcpf_") {
		t.Fatalf("attachment: %+v", att)
	}
	resp, _ = http.Get(att.DownloadURL) // no key
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(body) != "%PDF-receipt" ||
		resp.Header.Get("Content-Type") != "application/octet-stream" || !strings.HasPrefix(resp.Header.Get("Content-Disposition"), "attachment") ||
		resp.Header.Get("X-Content-Type-Options") != "nosniff" || resp.Header.Get("X-FCP-Content-Type") != "application/pdf" {
		t.Fatalf("download: %d %q %v", resp.StatusCode, body, resp.Header)
	}

	var ag struct{ Agents []store.AgentInfo }
	e.do("GET", "/v1/agents", e.hh, nil, 200, &ag)
	if ag.Agents[1].PendingFiles != 2 || ag.Agents[1].PendingFileBytes != int64(len("%PDF-receipt")+4) {
		t.Fatalf("pending file counts: %+v", ag.Agents)
	}
	e.inbox(a.Key, sent.ID) // ack: both files freed
	if resp, _ := http.Get(att.DownloadURL); resp.StatusCode != 404 {
		t.Fatalf("download after ack: %d", resp.StatusCode)
	}

	// MCP: inline base64 for a small file.
	var f struct {
		ID string `json:"file_id"`
	}
	if isErr, text := e.mcp(j.Key, "upload_file", map[string]any{"name": "note.txt", "content_base64": "aGVsbG8="}, &f); isErr || f.ID == "" {
		t.Fatalf("inline upload: %s", text)
	}
	e.mcp(j.Key, "send_message", map[string]any{"to_agent_id": a.ID, "kind": "notice", "body": "see note", "attachments": []string{f.ID}}, nil)
	var in inbox
	e.mcp(a.Key, "check_inbox", map[string]any{}, &in)
	in.Messages = fromAgents(in.Messages)
	if len(in.Messages) != 1 || len(in.Messages[0].Attachments) != 1 {
		t.Fatalf("mcp inbox: %+v", in)
	}
	resp, _ = http.Get(in.Messages[0].Attachments[0].DownloadURL)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "hello" {
		t.Fatalf("mcp download: %q", body)
	}
}

// TestSystemNotices: joins and release announcements arrive as kind "system"
// from "FCP", once.
func TestSystemNotices(t *testing.T) {
	st, _, _ := store.Open(filepath.Join(t.TempDir(), "k.json"))
	ts := httptest.NewServer(New(st, Config{}))
	defer ts.Close()
	_, hh, _ := st.CreateHousehold()
	e := &env{t: t, url: ts.URL, hh: hh}
	j := e.mint("Jamie's Muse")
	e.mint("Alex's Grok")
	if err := AnnounceRelease(st, "v0.3.0", ts.URL); err != nil {
		t.Fatal(err)
	}
	AnnounceRelease(st, "v0.3.0", ts.URL) // idempotent
	var in inbox
	e.do("GET", "/v1/inbox", j.Key, nil, 200, &in)
	if len(in.Messages) != 2 {
		t.Fatalf("want join + release notices, got %+v", in.Messages)
	}
	join, rel := in.Messages[0], in.Messages[1]
	if join.Kind != "system" || join.FromName != "FCP" || join.From != nil || !strings.Contains(join.Body, "Alex's Grok joined") {
		t.Fatalf("join notice: %+v", join)
	}
	if !strings.Contains(rel.Body, "FCP was updated to v0.3.0") || !strings.Contains(rel.Body, ts.URL+"/agents.md") {
		t.Fatalf("release notice: %+v", rel)
	}
	e.do("POST", "/v1/messages", j.Key, map[string]any{"kind": "system", "body": "spoof"}, 400, nil)
}
