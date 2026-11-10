package server

import (
	"context"
	"net/http"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type bearerRT string

func (k bearerRT) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+string(k))
	return http.DefaultTransport.RoundTrip(r)
}

// TestStockMCPClient drives the endpoint with an off-the-shelf MCP client,
// which offers a newer protocol version than FCP's newest: FCP must answer
// with 2025-11-25 rather than reject it.
func TestStockMCPClient(t *testing.T) {
	e := setup(t)
	a := e.mint("Claude Code")
	ctx := context.Background()
	c := mcp.NewClient(&mcp.Implementation{Name: "stock", Version: "1"}, nil)
	cs, err := c.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint: e.url + "/mcp", HTTPClient: &http.Client{Transport: bearerRT(a.Key)}, DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	if v := cs.InitializeResult().ProtocolVersion; v != "2025-11-25" {
		t.Fatalf("negotiated %s", v)
	}
	tools, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"send_message": true, "check_inbox": true, "list_agents": true, "get_watch_url": true, "set_webhook": true, "upload_file": true}
	if len(tools.Tools) != len(want) {
		t.Fatalf("tools: %d", len(tools.Tools))
	}
	for _, tl := range tools.Tools {
		if !want[tl.Name] {
			t.Fatalf("unexpected tool %s", tl.Name)
		}
	}
	b := e.mint("Other")
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "send_message", Arguments: map[string]any{"to_agent_id": b.ID, "kind": "notice", "body": "smoke"}})
	if err != nil || res.IsError {
		t.Fatalf("send_message: %v %+v", err, res)
	}
	if got := e.inbox(b.Key, 0); len(got) != 1 || got[0].Body != "smoke" || got[0].FromName != "Claude Code" {
		t.Fatalf("delivered: %+v", got)
	}
}
