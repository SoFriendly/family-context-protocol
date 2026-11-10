// Package mcp serves FCP's tools over MCP Streamable HTTP.
package mcp

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/sofriendly/family-context-protocol/internal/store"
)

// Versions FCP negotiates, newest first (SPEC §6.1).
var versions = []string{"2025-11-25", "2025-06-18", "2025-03-26"}

type agentKey struct{}

// Handler returns the /mcp endpoint. allow(key, write) applies rate limits.
// Options is what the MCP endpoint needs from the server.
type Options struct {
	Allow           func(key string, write bool) bool // rate limits
	ContractVersion string
	Base            func(*http.Request) string // public origin, for watch URLs
	ValidateWebhook func(url string) error
}

func Handler(st *store.Store, o Options) http.Handler {
	sdk := mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		return newServer(st, r.Context().Value(agentKey{}).(*store.Agent), func(write bool) bool {
			return o.Allow(bearer(r), write)
		}, o, o.Base(r))
	}, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true,
		// The SDK's DNS-rebinding guard rejects any non-localhost Host when we
		// listen on loopback, which is exactly the setup behind Tailscale Funnel
		// or a reverse proxy. It protects unauthenticated local servers; ours
		// demands an agent key before the SDK sees the request.
		DisableLocalhostProtection: true})

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := bearer(r)
		if strings.HasPrefix(key, "fcph_") {
			writeErr(w, 401, "agent key required", "the MCP endpoint takes an agent key (fcpa_…), not the household key; mint one with POST /v1/agents")
			return
		}
		a, err := st.AgentByKey(key)
		if err != nil {
			writeErr(w, 401, "unknown agent key", "send Authorization: Bearer <fcpa_ agent key>; the key may have been rotated or revoked")
			return
		}
		if r.Method == http.MethodPost && !precheck(w, r) {
			return
		}
		sdk.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), agentKey{}, a)))
	})
}

// precheck answers malformed JSON and unsupported methods with standard
// JSON-RPC errors (the SDK answers them in plain text), and hands out a
// Mcp-Session-Id on initialize. Sessions are stateless: any id is accepted back.
func precheck(w http.ResponseWriter, r *http.Request) bool {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 2<<20))
	if err != nil {
		writeErr(w, 413, "payload too large", "MCP requests are capped at 2 MB; upload big files with upload_file's upload_url")
		return false
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	var msg struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
	}
	if trimmed := bytes.TrimSpace(body); len(trimmed) > 0 && trimmed[0] == '[' {
		return true // batch: let the SDK decide
	}
	if err := json.Unmarshal(body, &msg); err != nil {
		rpcErr(w, 400, nil, -32700, "parse error: body is not valid JSON")
		return false
	}
	switch {
	case msg.Method == "initialize":
		id := r.Header.Get("Mcp-Session-Id")
		if id == "" {
			id = rand.Text()
		}
		w.Header().Set("Mcp-Session-Id", id)
	case msg.Method == "", msg.Method == "ping", msg.Method == "tools/list", msg.Method == "tools/call",
		msg.Method == "logging/setLevel", strings.HasPrefix(msg.Method, "notifications/"):
	default:
		rpcErr(w, 200, msg.ID, -32601, "method not found: "+msg.Method)
		return false
	}
	return true
}

func rpcErr(w http.ResponseWriter, status int, id json.RawMessage, code int, msg string) {
	if id == nil {
		id = json.RawMessage("null")
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": msg}})
}

func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if len(h) > 7 && strings.EqualFold(h[:7], "bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return ""
}

func writeErr(w http.ResponseWriter, status int, code, hint string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": code, "hint": hint})
}

// fail is an application error as a tool result, in the { error, hint } shape.
func fail(code, hint string) (*mcp.CallToolResult, any, error) {
	b, _ := json.Marshal(map[string]string{"error": code, "hint": hint})
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: string(b)}}}, nil, nil
}

func storeFail(err error) (*mcp.CallToolResult, any, error) {
	switch {
	case errors.Is(err, store.ErrStorageFull):
		return fail("file storage full", "the server is holding 200 MB of undelivered files; try again after recipients check their inboxes")
	case errors.Is(err, store.ErrInboxFull):
		return fail("recipient inbox full", "the recipient has 1000 unread messages; try again after it checks its inbox")
	case errors.Is(err, store.ErrBadRequest), errors.Is(err, store.ErrNotFound):
		return fail("bad request", err.Error())
	}
	return nil, nil, err
}

func limited() (*mcp.CallToolResult, any, error) {
	return fail("rate limited", "slow down: 60 reads/min and 30 writes/min per key (429)")
}

type (
	sendIn struct {
		To          string   `json:"to_agent_id" jsonschema:"recipient agent id from list_agents (required; use broadcast_message to message every other agent)"`
		Kind        string   `json:"kind" jsonschema:"update (FYI, something changed), question (expects a reply), task (expects action, then a reply), notice (heads-up), or reply (answers a message: set re)"`
		Body        string   `json:"body" jsonschema:"the message, markdown, max 256 KB"`
		Re          *int64   `json:"re,omitempty" jsonschema:"id of the message this replies to"`
		Attachments []string `json:"attachments,omitempty" jsonschema:"file ids from upload_file to attach (at most 10)"`
	}
	broadcastIn struct {
		Kind        string   `json:"kind" jsonschema:"update, question, task, notice, or reply (set re)"`
		Body        string   `json:"body" jsonschema:"the message, markdown, max 256 KB"`
		Re          *int64   `json:"re,omitempty" jsonschema:"id of the message this replies to"`
		Attachments []string `json:"attachments,omitempty" jsonschema:"file ids from upload_file to attach (at most 10)"`
	}
	uploadIn struct {
		Name          string `json:"name" jsonschema:"file name, e.g. itinerary.pdf"`
		ContentType   string `json:"content_type,omitempty" jsonschema:"MIME type, e.g. application/pdf"`
		Size          int64  `json:"size,omitempty" jsonschema:"size in bytes (max 25 MB). FCP returns an upload_url to PUT exactly these bytes to."`
		ContentBase64 string `json:"content_base64,omitempty" jsonschema:"for small files (up to 1 MB) only: the file itself, base64-encoded, instead of size"`
	}
	inboxIn struct {
		After int64 `json:"after,omitempty" jsonschema:"id of the last message you have fully handled; those and older are deleted from the server. Omit on first call."`
	}
	webhookIn struct {
		URL string `json:"url" jsonschema:"HTTPS URL that FCP should POST each new message to, the moment it arrives. Empty string removes the webhook."`
	}
	empty struct{}
)

func newServer(st *store.Store, a *store.Agent, allow func(write bool) bool, o Options, base string) *mcp.Server {
	contractVersion := o.ContractVersion
	s := mcp.NewServer(&mcp.Implementation{Name: "fcp", Version: "1"}, &mcp.ServerOptions{
		SupportedProtocolVersions: versions,
		Instructions:              "Family Context Protocol: a message bus between a household's agents. check_inbox to receive (pass after=<last id you handled> to delete handled messages); send_message to tell or ask one agent something (broadcast_message for all of them); reply with kind=reply and re=<message id>. The server keeps nothing after delivery, so remember what you need.",
	})

	mcp.AddTool(s, &mcp.Tool{Name: "send_message", Description: "Send one other agent a message (to_agent_id is required): an update, question, task, notice, or a reply to one of theirs. To message every other agent at once, use broadcast_message."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in sendIn) (*mcp.CallToolResult, any, error) {
			if !allow(true) {
				return limited()
			}
			m, to, err := st.Send(a, in.To, in.Kind, in.Body, in.Re, in.Attachments...)
			if err != nil {
				return storeFail(err)
			}
			return nil, map[string]any{"id": m.ID, "delivered_to": to, "created_at": m.CreatedAt}, nil
		})

	mcp.AddTool(s, &mcp.Tool{Name: "broadcast_message", Description: "Send the same message to every other agent in the household. Use it only when the message really is for all of them; otherwise use send_message."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in broadcastIn) (*mcp.CallToolResult, any, error) {
			if !allow(true) {
				return limited()
			}
			m, to, err := st.Broadcast(a, in.Kind, in.Body, in.Re, in.Attachments...)
			if err != nil {
				return storeFail(err)
			}
			return nil, map[string]any{"id": m.ID, "delivered_to": to, "created_at": m.CreatedAt}, nil
		})

	mcp.AddTool(s, &mcp.Tool{Name: "check_inbox", Description: "Get messages waiting for you, oldest first. Pass after=<id of the last message you handled> to delete it and everything older from the server."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in inboxIn) (*mcp.CallToolResult, any, error) {
			if !allow(false) {
				return limited()
			}
			return nil, map[string]any{"messages": store.WithBase(st.Inbox(a, in.After), base), "contract_version": contractVersion}, nil
		})

	mcp.AddTool(s, &mcp.Tool{Name: "list_agents", Description: "List the agents in your household (ids to address messages to), including you."},
		func(ctx context.Context, _ *mcp.CallToolRequest, _ empty) (*mcp.CallToolResult, any, error) {
			if !allow(false) {
				return limited()
			}
			as, err := st.ListAgents(a.HouseholdID)
			if err != nil {
				return storeFail(err)
			}
			return nil, map[string]any{"agents": as, "you": a.ID, "contract_version": contractVersion}, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "upload_file", Description: "Upload a file to attach to a message. Small files (up to 1 MB): pass content_base64 and get a file_id back. Larger files (up to 25 MB): pass size, then PUT exactly those bytes to the returned upload_url (no key needed, works once, 15 minutes). Then pass the file_id in send_message's attachments."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in uploadIn) (*mcp.CallToolResult, any, error) {
			if !allow(true) {
				return limited()
			}
			if in.ContentBase64 != "" {
				data, err := base64.StdEncoding.DecodeString(in.ContentBase64)
				if err != nil {
					return fail("bad request", "content_base64 is not valid base64")
				}
				if len(data) > store.MaxInline {
					return fail("too large for inline", "inline uploads are capped at 1 MB; pass size instead and PUT the bytes to upload_url")
				}
				att, err := st.AddFile(a, in.Name, in.ContentType, data)
				if err != nil {
					return storeFail(err)
				}
				return nil, map[string]any{"file_id": att.ID, "name": att.Name, "size": att.Size}, nil
			}
			id, tok, err := st.NewUpload(a, in.Name, in.ContentType, in.Size)
			if err != nil {
				return storeFail(err)
			}
			return nil, map[string]any{"file_id": id, "upload_url": base + "/v1/upload/" + tok, "upload_method": "PUT", "expires_in_seconds": 900}, nil
		})

	mcp.AddTool(s, &mcp.Tool{Name: "get_watch_url", Description: "Get a private URL that needs no key and returns only {waiting, newest_id} for your inbox: a count, never content. Point a lightweight watcher or poller at it, and call check_inbox when waiting > 0. Each call issues a new URL; the previous one stops working."},
		func(ctx context.Context, _ *mcp.CallToolRequest, _ empty) (*mcp.CallToolResult, any, error) {
			if !allow(true) {
				return limited()
			}
			tok, err := st.NewWatchToken(a)
			if err != nil {
				return storeFail(err)
			}
			return nil, map[string]any{"watch_url": base + "/v1/watch/" + tok}, nil
		})

	mcp.AddTool(s, &mcp.Tool{Name: "set_webhook", Description: "Have FCP POST each new message to your URL the moment it arrives (for agents that can receive HTTP calls). Returns a secret: verify X-FCP-Signature = sha256=HMAC-SHA256(secret, X-FCP-Timestamp + \".\" + body). Messages also stay in your inbox until acknowledged."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in webhookIn) (*mcp.CallToolResult, any, error) {
			if !allow(true) {
				return limited()
			}
			if in.URL != "" {
				if err := o.ValidateWebhook(in.URL); err != nil {
					return fail("bad webhook url", err.Error())
				}
			}
			secret, err := st.SetWebhook(a, in.URL)
			if err != nil {
				return storeFail(err)
			}
			if in.URL == "" {
				return nil, map[string]any{"webhook_url": nil}, nil
			}
			return nil, map[string]any{"webhook_url": in.URL, "secret": secret}, nil
		})
	return s
}
