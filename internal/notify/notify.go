// Package notify delivers webhooks: when a message lands in an agent's inbox
// and that agent registered a URL, FCP POSTs the message there right away.
// Webhooks are a nudge; the inbox stays the source of truth, so a failed
// delivery loses nothing.
package notify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"syscall"
	"time"

	"github.com/sofriendly/family-context-protocol/internal/store"
)

// Notifier sends webhooks. AllowPrivate permits http:// and private/loopback
// addresses, for webhooks that live on the operator's own network.
type Notifier struct {
	AllowPrivate bool
	UserAgent    string
	client       *http.Client
	sem          chan struct{}
	retry        []time.Duration
}

func New(allowPrivate bool, version string) *Notifier {
	n := &Notifier{AllowPrivate: allowPrivate, UserAgent: "fcp/" + version, sem: make(chan struct{}, 8),
		retry: []time.Duration{0, 5 * time.Second, 30 * time.Second}}
	dialer := &net.Dialer{Timeout: 10 * time.Second, Control: n.checkAddr}
	n.client = &http.Client{
		Timeout:       15 * time.Second,
		Transport:     &http.Transport{DialContext: dialer.DialContext, TLSHandshakeTimeout: 10 * time.Second},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return n
}

var errPrivate = errors.New("webhook address is private or local; set FCP_WEBHOOK_ALLOW_PRIVATE=1 to allow it")

// checkAddr runs at dial time, after DNS, so a hostname can't smuggle in a
// private address.
func (n *Notifier) checkAddr(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if !n.AllowPrivate && (ip == nil || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast() || isCGNAT(ip)) {
		return errPrivate
	}
	return nil
}

func isCGNAT(ip net.IP) bool { // 100.64.0.0/10, which includes Tailscale addresses
	v4 := ip.To4()
	return v4 != nil && v4[0] == 100 && v4[1]&0xc0 == 64
}

// Validate checks a webhook URL before it's stored.
func (n *Notifier) Validate(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || len(raw) > 2048 {
		return errors.New("webhook_url must be an absolute URL")
	}
	if u.Scheme != "https" && !(n.AllowPrivate && u.Scheme == "http") {
		return errors.New("webhook_url must use https")
	}
	if ip := net.ParseIP(u.Hostname()); ip != nil && n.checkAddr("", net.JoinHostPort(ip.String(), "443"), nil) != nil {
		return errPrivate
	}
	return nil
}

// Deliver POSTs m to the recipient's webhook in the background, retrying a
// couple of times. Safe to call for agents without a webhook (no-op).
func (n *Notifier) Deliver(to store.Agent, m store.Message) {
	if to.WebhookURL == "" {
		return
	}
	body, _ := json.Marshal(map[string]any{"event": "message", "agent_id": to.ID, "message": m})
	go func() {
		n.sem <- struct{}{}
		defer func() { <-n.sem }()
		var err error
		for _, wait := range n.retry {
			time.Sleep(wait)
			if err = n.post(to.WebhookURL, to.WebhookSecret, body); err == nil {
				return
			}
		}
		log.Printf("webhook for agent %s failed: %v (the message is still in its inbox)", to.ID, err)
	}()
}

func (n *Notifier) post(target, secret string, body []byte) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST", target, bytes.NewReader(body))
	if err != nil {
		return err
	}
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", n.UserAgent)
	req.Header.Set("X-FCP-Timestamp", ts)
	req.Header.Set("X-FCP-Signature", "sha256="+Sign(secret, ts, body))
	resp, err := n.client.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("webhook answered %s", resp.Status)
	}
	return nil
}

// Sign is the webhook signature: hex HMAC-SHA256 of "<timestamp>.<body>".
func Sign(secret, ts string, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(ts + "."))
	m.Write(body)
	return hex.EncodeToString(m.Sum(nil))
}
