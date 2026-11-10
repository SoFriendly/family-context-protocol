package update

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestNewer(t *testing.T) {
	for _, c := range []struct {
		cur, latest string
		want        bool
	}{
		{"v0.2.0", "v0.2.1", true},
		{"v0.2.1", "v0.2.1", false},
		{"v0.10.0", "v0.9.9", false},
		{"v1.0.0", "v0.99.0", false},
		{"dev", "v0.1.0", true},
		{"v0.2.0", "garbage", false},
	} {
		if got := Newer(c.cur, c.latest); got != c.want {
			t.Errorf("Newer(%s, %s) = %v", c.cur, c.latest, got)
		}
	}
}

func TestInstall(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	PublicKey = base64.StdEncoding.EncodeToString(pub)
	name := fmt.Sprintf("fcp-%s-%s", runtime.GOOS, runtime.GOARCH)
	bin := []byte("new binary")
	files := map[string][]byte{}
	release := func(binary []byte, signer ed25519.PrivateKey) {
		h := sha256.Sum256(bin)
		sums := []byte(hex.EncodeToString(h[:]) + "  " + name + "\n")
		files["/LATEST"] = []byte("v9.9.9\n")
		files["/v9.9.9/SHA256SUMS"] = sums
		files["/v9.9.9/SHA256SUMS.sig"] = []byte(base64.StdEncoding.EncodeToString(ed25519.Sign(signer, sums)))
		files["/v9.9.9/"+name] = binary
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, ok := files[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write(b)
	}))
	defer ts.Close()
	BaseURL = ts.URL

	target := filepath.Join(t.TempDir(), "fcp")
	os.WriteFile(target, []byte("old"), 0o755)

	release(bin, priv)
	if v, err := Latest(); err != nil || v != "v9.9.9" {
		t.Fatal(v, err)
	}

	_, other, _ := ed25519.GenerateKey(rand.Reader)
	release(bin, other)
	if err := Install("v9.9.9", target); err == nil || !strings.Contains(err.Error(), "signature") {
		t.Fatalf("wrong signer: %v", err)
	}
	release([]byte("evil"), priv)
	if err := Install("v9.9.9", target); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("tampered binary: %v", err)
	}
	if b, _ := os.ReadFile(target); string(b) != "old" {
		t.Fatal("binary replaced despite failed verification")
	}

	release(bin, priv)
	if err := Install("v9.9.9", target); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(target)
	fi, _ := os.Stat(target)
	if string(b) != "new binary" || fi.Mode().Perm() != 0o755 {
		t.Fatalf("installed %q mode %v", b, fi.Mode())
	}
}
