package main

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"mime"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// Who may call the server. It runs Devin on whatever task it is handed, so a caller is held to
// three things:
//
//   - Not a browser. A page can POST to 127.0.0.1 without asking (a text/plain body needs no
//     preflight), and a name rebound to 127.0.0.1 lets it read the answers too. So a request with
//     an Origin, a Host that is not this machine, or a body that is not JSON is turned away.
//   - Holding the token: `Authorization: Bearer <token>`, the token being the contents of
//     $XDG_STATE_HOME/claude-devin-acp/token, made on the first start and readable by the user
//     alone. OpenCode reads it from there (`{file:…}` in its config); a process that cannot read
//     the user's files, or an agent's shell sandboxed away from it, cannot call in.
//   - The few routes left open without it (`/limit`, `/compact/{session}`) are ones `wait-limit`
//     calls from inside such a sandbox, and say or do nothing that matters.

// stateDir is the server's own directory under $XDG_STATE_HOME, made private.
func stateDir() (string, error) {
	state := os.Getenv("XDG_STATE_HOME")
	if state == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		state = filepath.Join(home, ".local", "state")
	}
	return privateDir(filepath.Join(state, "claude-devin-acp"))
}

// cacheDir is the server's own directory under $XDG_CACHE_HOME, made private: it holds what the
// sessions were asked and did.
func cacheDir() (string, error) {
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return privateDir(filepath.Join(cache, "claude-devin-acp"))
}

// privateDir makes dir, and makes it the user's alone if it was there before.
func privateDir(dir string) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, os.Chmod(dir, 0o700)
}

// loadToken reads the token, making it on the first start.
func loadToken() (string, error) {
	dir, err := stateDir()
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, "token")
	if raw, err := os.ReadFile(path); err == nil {
		if token := strings.TrimSpace(string(raw)); len(token) >= 32 {
			return token, nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	token := randomToken(32)
	if err := os.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
		return "", err
	}
	return token, nil
}

// clientToken is the token for the `status` command, empty when it cannot be read.
func clientToken() string {
	dir, err := stateDir()
	if err != nil {
		return ""
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "token"))
	return strings.TrimSpace(string(raw))
}

func randomToken(bytes int) string {
	b := make([]byte, bytes)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// sameSecret compares two secrets in constant time.
func sameSecret(given, want string) bool {
	return want != "" && subtle.ConstantTimeCompare([]byte(given), []byte(want)) == 1
}

// guard is next, behind the checks above: the token's too, unless open.
func guard(token string, open bool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := fromHere(r); err != nil {
			http.Error(w, err.Error(), http.StatusForbidden)
			return
		}
		if !open {
			given, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
			if !sameSecret(given, token) {
				w.Header().Set("WWW-Authenticate", "Bearer")
				http.Error(w, "the token is required: Authorization: Bearer <$XDG_STATE_HOME/claude-devin-acp/token>", http.StatusUnauthorized)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// fromHere is why a request is not one from a program on this machine, nil when it is.
func fromHere(r *http.Request) error {
	if r.Header.Get("Origin") != "" {
		return errors.New("requests from browsers are not taken")
	}
	host, _, err := net.SplitHostPort(r.Host)
	if err != nil {
		host = r.Host
	}
	if ip := net.ParseIP(strings.Trim(host, "[]")); ip == nil || !ip.IsLoopback() {
		if host != "localhost" {
			return fmt.Errorf("host %q is not this machine", r.Host)
		}
	}
	if r.Method == http.MethodPost {
		if media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || media != "application/json" {
			return errors.New("the body must be application/json")
		}
	}
	return nil
}
