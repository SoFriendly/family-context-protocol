// Command fcp is the Family Context Protocol server.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/sofriendly/family-context-protocol/internal/server"
	"github.com/sofriendly/family-context-protocol/internal/store"
	"github.com/sofriendly/family-context-protocol/internal/ui"
	"github.com/sofriendly/family-context-protocol/internal/update"
)

var version = "dev" // set with -ldflags "-X main.version=..."

const usage = `fcp — Family Context Protocol server

Usage:
  fcp serve  [--listen 127.0.0.1:8080] [--keys ./fcp-keys.json] [--public-url URL]
  fcp update [--check]                    check getfcp.com for a newer release and install it
  fcp health [--listen 127.0.0.1:8080]    exit 0 if /healthz answers (for Docker)
  fcp version

Env: FCP_LISTEN, FCP_KEYS, FCP_PUBLIC_URL, FCP_ADMIN_TOKEN. Flags override env.
`

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	fs := flag.NewFlagSet(os.Args[1], flag.ExitOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	listen := fs.String("listen", env("FCP_LISTEN", "127.0.0.1:8080"), "listen address")
	keys := fs.String("keys", env("FCP_KEYS", "./fcp-keys.json"), "credentials file (key hashes only)")
	public := fs.String("public-url", env("FCP_PUBLIC_URL", ""), "public base URL, for display")
	checkOnly := fs.Bool("check", false, "update: only report whether a newer release exists")
	fs.Parse(os.Args[2:])

	var err error
	switch os.Args[1] {
	case "serve":
		err = serve(*listen, *keys, *public)
	case "update":
		err = selfUpdate(*checkOnly)
	case "health":
		err = health(*listen)
	case "version":
		fmt.Println("fcp", version)
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "fcp:", err)
		os.Exit(1)
	}
}

func serve(listen, keysPath, public string) error {
	st, created, err := store.Open(keysPath)
	if err != nil {
		return err
	}

	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return fmt.Errorf("--listen %q: %w", listen, err)
	}
	explicit := public
	if public == "" {
		h := host
		if h == "" || h == "0.0.0.0" || h == "::" {
			h = "localhost"
		}
		public = "http://" + net.JoinHostPort(h, port)
	}
	public = strings.TrimRight(public, "/")

	if created {
		_, key, err := st.CreateHousehold()
		if err != nil {
			return err
		}
		keyFile := filepath.Join(filepath.Dir(keysPath), "household-key.txt")
		if err := os.WriteFile(keyFile, []byte(key+"\n"), 0o600); err != nil {
			return err
		}
		fmt.Printf("\nCreated a new household (credentials in %s).\n", keysPath)
		// Print the key only to an interactive terminal, so it never lands in
		// a log file or `docker logs`.
		if fi, err := os.Stdout.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 {
			fmt.Printf("\n  Household key: %s\n\n", key)
		}
		fmt.Printf("  The household key is in %s (0600). Save it in your password manager, then delete the file.\n\n", keyFile)
	}

	// A household with no agents yet gets a fresh setup link on every start, so
	// a lost or expired link is fixed by restarting.
	for _, hid := range st.EmptyHouseholds() {
		tok, exp, err := st.CreateInvite(hid)
		if err != nil {
			return err
		}
		rule := strings.Repeat("─", 60)
		fmt.Printf("\nSet up your AI assistant: copy everything between the lines and paste it into its chat.\n%s\n%s\n%s\n(The link works once and expires %s.)\n\n",
			rule, ui.PasteMessage(public+"/join/"+tok, ""), rule, exp.Format("Jan 2"))
		if u, err := url.Parse(public); err == nil && isLocal(u.Hostname()) {
			fmt.Println("  Note: this link points at this machine only. Cloud-based AI assistants can't reach it.")
			fmt.Println("  Expose FCP (e.g. `tailscale funnel --bg 8080`) and restart with --public-url https://<that address>.")
			fmt.Println()
		}
	}

	// The /admin "Install and restart" button asks for a restart once the new
	// binary is in place; we shut down cleanly and exec it in this same process.
	restart := make(chan struct{}, 1)
	h := server.New(st, server.Config{AdminToken: os.Getenv("FCP_ADMIN_TOKEN"), PublicURL: explicit, Version: version,
		AllowPrivateWebhooks: os.Getenv("FCP_WEBHOOK_ALLOW_PRIVATE") == "1",
		Restart: func() {
			select {
			case restart <- struct{}{}:
			default:
			}
		}})
	// Tell agents about this release (once per contract change) now that
	// webhooks are wired up.
	if err := server.AnnounceRelease(st, version, public); err != nil {
		return err
	}
	srv := &http.Server{Addr: listen, Handler: h, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 10 * time.Minute, WriteTimeout: 10 * time.Minute} // room for 25 MB attachments on slow links
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return err
	}
	log.Printf("fcp %s serving %s (admin %s/admin, MCP %s/mcp)", version, listen, public, public)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var restarting atomic.Bool
	done := make(chan struct{})
	go func() {
		defer close(done)
		select {
		case <-ctx.Done():
		case <-restart:
			restarting.Store(true)
		}
		shut, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(shut)
	}()
	if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	<-done // Shutdown has finished sending the last responses
	if restarting.Load() {
		exe, err := update.Executable()
		if err != nil {
			return err
		}
		log.Printf("restarting into the updated binary")
		return syscall.Exec(exe, os.Args, os.Environ())
	}
	return nil
}

// selfUpdate contacts getfcp.com, as the /admin update button does; nothing
// else in FCP calls out.
func selfUpdate(checkOnly bool) error {
	latest, err := update.Latest()
	if err != nil {
		return fmt.Errorf("checking for updates: %w", err)
	}
	if !update.Newer(version, latest) {
		fmt.Printf("fcp %s is up to date (latest is %s).\n", version, latest)
		return nil
	}
	fmt.Printf("A newer release is available: %s (you have %s).\n", latest, version)
	if checkOnly {
		fmt.Println("Run `fcp update` to install it.")
		return nil
	}
	exe, err := update.Executable()
	if err != nil {
		return err
	}
	if err := update.Install(latest, exe); err != nil {
		return err
	}
	fmt.Printf("Installed %s at %s (signature and checksum verified).\n", latest, exe)
	fmt.Println("Restart fcp to finish. A restart drops undelivered messages: check that /admin shows 0 waiting first.")
	return nil
}

func isLocal(host string) bool {
	ip := net.ParseIP(host)
	return host == "localhost" || (ip != nil && (ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified()))
}

func health(listen string) error {
	_, port, err := net.SplitHostPort(listen)
	if err != nil {
		return err
	}
	c := http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get("http://127.0.0.1:" + port + "/healthz")
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("healthz: %s", resp.Status)
	}
	return nil
}
