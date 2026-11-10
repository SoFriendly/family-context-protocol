package notify

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sofriendly/family-context-protocol/internal/store"
)

func TestValidate(t *testing.T) {
	n := New(false, "test")
	for raw, ok := range map[string]bool{
		"https://hooks.example.com/fcp": true,
		"http://hooks.example.com/fcp":  false,
		"https://127.0.0.1/x":           false,
		"https://10.0.0.5/x":            false,
		"https://100.101.102.103/x":     false, // tailnet
		"https://[::1]/x":               false,
		"not a url":                     false,
	} {
		if err := n.Validate(raw); (err == nil) != ok {
			t.Errorf("Validate(%q) = %v", raw, err)
		}
	}
	if err := New(true, "test").Validate("http://192.168.1.10:9000/hook"); err != nil {
		t.Errorf("allow-private: %v", err)
	}
}

func TestDeliverSignedAndPrivateBlocked(t *testing.T) {
	got := make(chan *http.Request, 1)
	bodies := make(chan []byte, 1)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got <- r
		bodies <- b
	}))
	defer ts.Close()
	m := store.Message{ID: 42, FromName: "Jamie's Muse", Kind: "task", Body: "Add parking"}
	to := store.Agent{ID: "a1", WebhookURL: ts.URL, WebhookSecret: "fcps_test"}

	// Default notifier refuses the loopback test server.
	blocked := New(false, "test")
	if err := blocked.post(ts.URL, "s", []byte("{}")); err == nil || !strings.Contains(err.Error(), "private") {
		t.Fatalf("loopback not blocked: %v", err)
	}

	n := New(true, "test")
	n.Deliver(to, m)
	select {
	case r := <-got:
		b := <-bodies
		if r.Header.Get("X-FCP-Signature") != "sha256="+Sign("fcps_test", r.Header.Get("X-FCP-Timestamp"), b) {
			t.Fatal("bad signature")
		}
		var p struct {
			Event   string
			Message store.Message
		}
		json.Unmarshal(b, &p)
		if p.Event != "message" || p.Message.ID != 42 || p.Message.Body != "Add parking" {
			t.Fatalf("payload: %s", b)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("webhook not delivered")
	}
}
