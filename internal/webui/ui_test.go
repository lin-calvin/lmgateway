package webui

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandlerServesAssets(t *testing.T) {
	srv := httptest.NewServer(http.StripPrefix("/ui", Handler()))
	defer srv.Close()

	for path, want := range map[string]string{
		"/ui/":       "lmgateway console",
		"/ui/app.js": "export",
		"/ui/api.js": "class Client",
	} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s: status %d", path, resp.StatusCode)
		}
		if !strings.Contains(string(body), want) {
			t.Errorf("GET %s: body does not contain %q (got %d bytes)", path, want, len(body))
		}
	}
}
