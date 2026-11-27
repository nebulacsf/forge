package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

// TestPublicConfigExposesBuildVersion pins the contract the UI footer relies on:
// GET /api/config carries the build stamp handed to New, so the version shown in
// the browser is the one the binary was compiled with and never a second read of
// the environment at runtime.
func TestPublicConfigExposesBuildVersion(t *testing.T) {
	for _, want := range []string{"v1.2.3-4-gabc1234-dirty", "dev", ""} {
		t.Run(want, func(t *testing.T) {
			cfg, st, _ := modelsFixture(t)
			app := httptest.NewServer(New(cfg, st, os.DirFS("."), want).Handler())
			defer app.Close()

			resp, err := http.Get(app.URL + "/api/config")
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d", resp.StatusCode)
			}
			var payload struct {
				Version string `json:"version"`
				Config  struct {
					Defaults struct{ Model string } `json:"defaults"`
				} `json:"config"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
				t.Fatal(err)
			}
			if payload.Version != want {
				t.Fatalf("version = %q, want %q", payload.Version, want)
			}
			if payload.Config.Defaults.Model == "" {
				t.Fatal("config payload lost the defaults")
			}
		})
	}
}
