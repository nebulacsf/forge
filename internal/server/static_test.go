package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// TestStaticAssetsAreLocalAndCached guards the offline UI contract: the
// vendored libraries must exist under web/vendor, index.html must not point at
// a CDN, and the cache policy must keep vendored files immutable while the app
// shell stays revalidated.
func TestStaticAssetsAreLocalAndCached(t *testing.T) {
	cfg, st, _ := modelsFixture(t)

	webRoot := os.DirFS("../../web")
	srv := httptest.NewServer(New(cfg, st, webRoot).Handler())
	defer srv.Close()

	cases := []struct {
		path      string
		wantCache string
	}{
		{"/vendor/bootstrap/bootstrap.min.css?v=5.3.3", "public, max-age=31536000, immutable"},
		{"/vendor/marked/marked.min.js?v=12.0.2", "public, max-age=31536000, immutable"},
		{"/js/app.js", "no-cache"},
		{"/css/app.css", "no-cache"},
		{"/", "no-cache"},
	}
	for _, tc := range cases {
		resp, err := http.Get(srv.URL + tc.path)
		if err != nil {
			t.Fatalf("GET %s: %v", tc.path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s: status %d, want 200", tc.path, resp.StatusCode)
		}
		if got := resp.Header.Get("Cache-Control"); got != tc.wantCache {
			t.Errorf("GET %s: Cache-Control %q, want %q", tc.path, got, tc.wantCache)
		}
	}

	index, err := os.ReadFile("../../web/index.html")
	if err != nil {
		t.Fatal(err)
	}
	for _, needle := range []string{"cdn.jsdelivr.net", "unpkg.com", "cdnjs.cloudflare.com"} {
		if strings.Contains(string(index), needle) {
			t.Errorf("index.html still references external CDN %q; the UI must load offline", needle)
		}
	}
}
