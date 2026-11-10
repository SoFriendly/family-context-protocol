// Package update checks getfcp.com for a newer release and installs it, but
// only when the operator asks: `fcp update`, or the button in /admin. FCP
// never checks on its own.
//
// Every release publishes SHA256SUMS and SHA256SUMS.sig (Ed25519). A download
// is installed only if the signature verifies against PublicKey, compiled into
// this binary, and the file's checksum matches. A compromised website can't
// push a binary that wasn't signed with the release key.
package update

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// PublicKey is the release-signing key (base64 Ed25519). See scripts/sign.
var PublicKey = "VQfKMF03ofzGAqe4W/RzeOA4xM3O+9Ux+AcmDKdidWg="

// BaseURL is where releases live; FCP_UPDATE_URL overrides it for testing.
var BaseURL = "https://getfcp.com/dl"

var client = &http.Client{Timeout: 2 * time.Minute}

func init() {
	if u := os.Getenv("FCP_UPDATE_URL"); u != "" {
		BaseURL = strings.TrimRight(u, "/")
	}
}

// Executable is the real path of the running binary, the one Install replaces.
func Executable() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(exe)
}

func get(url string) ([]byte, error) {
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 64<<20))
}

// Latest returns the newest released version, e.g. "v0.2.1".
func Latest() (string, error) {
	b, err := get(BaseURL + "/LATEST")
	if err != nil {
		return "", err
	}
	v := strings.TrimSpace(string(b))
	if _, ok := parse(v); !ok {
		return "", fmt.Errorf("unexpected version %q from %s/LATEST", v, BaseURL)
	}
	return v, nil
}

// Newer reports whether latest is newer than current. A dev build is always older.
func Newer(current, latest string) bool {
	c, okc := parse(current)
	l, okl := parse(latest)
	if !okl {
		return false
	}
	if !okc {
		return true
	}
	for i := range 3 {
		if l[i] != c[i] {
			return l[i] > c[i]
		}
	}
	return false
}

func parse(v string) ([3]int, bool) {
	var out [3]int
	parts := strings.Split(strings.TrimPrefix(v, "v"), ".")
	if len(parts) != 3 || !strings.HasPrefix(v, "v") {
		return out, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return out, false
		}
		out[i] = n
	}
	return out, true
}

// Install downloads version for this platform, verifies it, and atomically
// replaces the binary at target (normally os.Executable()).
func Install(version, target string) error {
	pub, err := base64.StdEncoding.DecodeString(PublicKey)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return errors.New("this build has no release public key; download the new version by hand from https://getfcp.com")
	}
	name := fmt.Sprintf("fcp-%s-%s", runtime.GOOS, runtime.GOARCH)
	dir := BaseURL + "/" + version
	sums, err := get(dir + "/SHA256SUMS")
	if err != nil {
		return err
	}
	sig, err := get(dir + "/SHA256SUMS.sig")
	if err != nil {
		return err
	}
	rawSig, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(sig)))
	if err != nil || !ed25519.Verify(pub, sums, rawSig) {
		return errors.New("release signature does not verify; refusing to install")
	}
	want := ""
	for _, line := range strings.Split(string(sums), "\n") {
		if f := strings.Fields(line); len(f) == 2 && f[1] == name {
			want = f[0]
		}
	}
	if want == "" {
		return fmt.Errorf("release %s has no binary for %s", version, name)
	}
	bin, err := get(dir + "/" + name)
	if err != nil {
		return err
	}
	if h := sha256.Sum256(bin); hex.EncodeToString(h[:]) != want {
		return errors.New("downloaded binary does not match its signed checksum; refusing to install")
	}
	tmp, err := os.CreateTemp(filepath.Dir(target), ".fcp-update-*")
	if err != nil {
		return fmt.Errorf("can't write next to %s (try sudo, or update by hand): %w", target, err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(bin); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o755); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), target)
}
