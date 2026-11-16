package lib

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestEmbeddedFrontendServesUnderscoreChunks(t *testing.T) {
	// SvelteKit may emit shared chunks whose filenames start with an underscore.
	// The Go embed default excludes those files unless the pattern opts in.
	chunks, err := filepath.Glob("web/_app/immutable/chunks/_*.js")
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) == 0 {
		t.Skip("frontend build did not produce an underscore-prefixed chunk")
	}
	for _, chunk := range chunks {
		name := "/_app/immutable/chunks/" + filepath.Base(chunk)
		request := httptest.NewRequest(http.MethodGet, name, nil)
		response := httptest.NewRecorder()
		serveSPA(response, request, http.NotFoundHandler())
		if response.Code != http.StatusOK || !strings.Contains(response.Header().Get("Content-Type"), "javascript") {
			t.Errorf("embedded chunk %s: status %d, content type %q", name, response.Code, response.Header().Get("Content-Type"))
		}
	}
}
