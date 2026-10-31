package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOnlyProgramsHereWithTheTokenGetIn(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	cases := []struct {
		name    string
		open    bool
		host    string
		headers map[string]string
		want    int
	}{
		{"with the token", false, "127.0.0.1:47821", map[string]string{"Authorization": "Bearer secret"}, http.StatusOK},
		{"localhost", false, "localhost:47821", map[string]string{"Authorization": "Bearer secret"}, http.StatusOK},
		{"without it", false, "127.0.0.1:47821", nil, http.StatusUnauthorized},
		{"with another", false, "127.0.0.1:47821", map[string]string{"Authorization": "Bearer guess"}, http.StatusUnauthorized},
		{"a page's text/plain POST", true, "127.0.0.1:47821", map[string]string{"Content-Type": "text/plain"}, http.StatusForbidden},
		{"a page's fetch", true, "127.0.0.1:47821", map[string]string{"Origin": "https://evil.example"}, http.StatusForbidden},
		{"a rebound name", true, "rebind.evil.example:47821", nil, http.StatusForbidden},
		{"open, from here", true, "127.0.0.1:47821", nil, http.StatusOK},
	}
	for _, c := range cases {
		body := strings.NewReader("{}")
		r := httptest.NewRequest(http.MethodPost, "http://"+c.host+"/mcp", body)
		r.Host = c.host
		r.Header.Set("Content-Type", "application/json")
		for k, v := range c.headers {
			r.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		guard("secret", c.open, ok).ServeHTTP(w, r)
		if w.Code != c.want {
			t.Errorf("%s: %d, want %d", c.name, w.Code, c.want)
		}
	}
}
