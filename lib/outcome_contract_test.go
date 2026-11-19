package lib

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func requireOutcome(t *testing.T, response *http.Response, code, data string) {
	t.Helper()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("decode outcome: %v (%s)", err, body)
	}
	if len(envelope) != 3 {
		t.Fatalf("outcome fields = %v, want exactly code/message/data", envelope)
	}
	var gotCode, message string
	if json.Unmarshal(envelope["code"], &gotCode) != nil ||
		json.Unmarshal(envelope["message"], &message) != nil ||
		gotCode != code ||
		message == "" ||
		string(envelope["data"]) != data {
		t.Fatalf("outcome = %s, want code %q and data %s", body, code, data)
	}
	if response.Header.Get("Content-Type") != "application/json" ||
		response.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("outcome headers: content-type=%q cache-control=%q", response.Header.Get("Content-Type"), response.Header.Get("Cache-Control"))
	}
}

func TestApplicationRouterUsesOutcomeForUnknownAndUnsupportedMethods(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/known", func(w http.ResponseWriter, _ *http.Request) {
		writeOutcome(w, http.StatusOK, "test.listed", "Loaded.", nil)
	})

	for _, tc := range []struct {
		method string
		path   string
		status int
		code   string
		allow  string
	}{
		{method: http.MethodGet, path: "/api/missing", status: http.StatusNotFound, code: "request.not_found"},
		{method: http.MethodPost, path: "/api/known", status: http.StatusMethodNotAllowed, code: "request.method_not_allowed", allow: "GET, HEAD"},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			request := httptest.NewRequest(tc.method, tc.path, nil)
			response := httptest.NewRecorder()
			serveApplicationRoute(response, request, mux)
			result := response.Result()
			defer result.Body.Close()
			requireOutcome(t, result, tc.code, "null")
			if result.StatusCode != tc.status {
				t.Fatalf("status = %d, want %d", result.StatusCode, tc.status)
			}
			if result.Header.Get("Allow") != tc.allow {
				t.Fatalf("Allow = %q, want %q", result.Header.Get("Allow"), tc.allow)
			}
		})
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serveApplicationRoute(w, r, mux)
	}))
	defer server.Close()
	response, err := server.Client().Head(server.URL + "/api/known")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || len(body) != 0 ||
		response.Header.Get("Content-Type") != "application/json" ||
		response.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("HEAD response: status=%d body=%q content-type=%q cache-control=%q",
			response.StatusCode, body, response.Header.Get("Content-Type"), response.Header.Get("Cache-Control"))
	}
}

func TestApplicationPathRecognizesReservedRootsAndDescendants(t *testing.T) {
	for _, path := range []string{"/api", "/api/unknown", "/opaque", "/opaque/unknown", "/webhooks", "/webhooks/unknown"} {
		if !isApplicationPath(path) {
			t.Errorf("%q was not routed as an application path", path)
		}
	}
	for _, path := range []string{"/apiary", "/webhooks-extra", "/dashboard"} {
		if isApplicationPath(path) {
			t.Errorf("%q was incorrectly routed as an application path", path)
		}
	}
}

func TestApplicationRouteOutcomeMatrixUsesSafeCodesAndHeaders(t *testing.T) {
	dir := t.TempDir()
	app, err := New(Config{DatabasePath: filepath.Join(dir, "state.db"), OPAQUEKeyPath: filepath.Join(dir, "opaque.key")})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	server := httptest.NewTLSServer(app.Handler())
	defer server.Close()

	const resource = "0123456789abcdef0123456789abcdef"
	routes := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/api/session"},
		{http.MethodPost, "/api/logout"},
		{http.MethodGet, "/api/projects"},
		{http.MethodPost, "/api/projects"},
		{http.MethodGet, "/api/projects/" + resource + "/refs"},
		{http.MethodPost, "/api/projects/" + resource + "/refresh"},
		{http.MethodPost, "/api/projects/" + resource + "/key"},
		{http.MethodPost, "/api/projects/" + resource + "/cache/recreate"},
		{http.MethodPost, "/api/projects/" + resource + "/delete"},
		{http.MethodGet, "/api/projects/" + resource + "/lfs"},
		{http.MethodPost, "/api/projects/" + resource + "/lfs"},
		{http.MethodGet, "/api/projects/" + resource + "/github-webhook"},
		{http.MethodPost, "/api/projects/" + resource + "/github-webhook"},
		{http.MethodGet, "/api/projects/" + resource + "/gitea-webhook"},
		{http.MethodPost, "/api/projects/" + resource + "/gitea-webhook"},
		{http.MethodGet, "/api/deployments"},
		{http.MethodGet, "/api/deployments/root"},
		{http.MethodPost, "/api/deployments"},
		{http.MethodPost, "/api/deployments/" + resource + "/deploy"},
		{http.MethodGet, "/api/deployments/" + resource + "/history"},
		{http.MethodPost, "/api/deployments/" + resource + "/rollback"},
		{http.MethodPost, "/api/deployments/" + resource + "/pause"},
		{http.MethodPost, "/api/deployments/" + resource + "/resume"},
		{http.MethodPost, "/api/deployments/" + resource + "/hard-redeploy"},
		{http.MethodPost, "/api/deployments/" + resource + "/delete"},
		{http.MethodPost, "/api/deployments/" + resource + "/move"},
		{http.MethodPost, "/api/deployments/" + resource + "/target"},
		{http.MethodGet, "/api/deployments/" + resource + "/jobs"},
		{http.MethodGet, "/api/repository-keys"},
		{http.MethodPost, "/api/repository-keys"},
		{http.MethodPost, "/api/repository-keys/" + resource + "/delete"},
		{http.MethodGet, "/api/ssh-hosts"},
		{http.MethodPost, "/api/ssh-hosts/approve"},
		{http.MethodPost, "/api/ssh-hosts/revoke"},
		{http.MethodGet, "/api/events"},
		{http.MethodPost, "/opaque/register/start"},
		{http.MethodPost, "/opaque/register/finish"},
		{http.MethodPost, "/opaque/login/start"},
		{http.MethodPost, "/opaque/login/finish"},
		{http.MethodPost, "/opaque/password/change/start"},
		{http.MethodPost, "/opaque/password/change/finish"},
		{http.MethodPost, "/webhooks/github/" + resource},
		{http.MethodPost, "/webhooks/gitea/" + resource},
	}
	type expectation struct {
		status int
		code   string
	}
	special := map[string]expectation{
		"POST /api/logout":                    {http.StatusOK, "auth.logged_out"},
		"GET /api/events":                     {http.StatusBadRequest, "request.invalid"},
		"POST /opaque/password/change/start":  {http.StatusBadRequest, "auth.protocol_invalid"},
		"POST /opaque/password/change/finish": {http.StatusBadRequest, "auth.protocol_invalid"},
		"POST /webhooks/github/" + resource:   {http.StatusNotFound, "webhook.not_configured"},
		"POST /webhooks/gitea/" + resource:    {http.StatusNotFound, "webhook.not_configured"},
	}
	for _, route := range []string{
		"POST /opaque/register/start", "POST /opaque/register/finish",
		"POST /opaque/login/start", "POST /opaque/login/finish",
	} {
		special[route] = expectation{http.StatusBadRequest, "auth.protocol_invalid"}
	}
	for _, route := range routes {
		t.Run(fmt.Sprintf("%s %s", route.method, route.path), func(t *testing.T) {
			req, err := http.NewRequest(route.method, server.URL+route.path, strings.NewReader("{}"))
			if err != nil {
				t.Fatal(err)
			}
			if route.method == http.MethodPost {
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("Origin", server.URL)
			}
			if strings.HasPrefix(route.path, "/webhooks/") {
				req.Header.Set("X-GitHub-Delivery", "delivery-1")
				req.Header.Set("X-GitHub-Event", "push")
				req.Header.Set("X-Hub-Signature-256", "sha256="+strings.Repeat("0", 64))
				req.Header.Set("X-Gitea-Delivery", "delivery-1")
				req.Header.Set("X-Gitea-Event", "push")
				req.Header.Set("X-Gitea-Signature", strings.Repeat("0", 64))
			}
			wantStatus, wantCode := http.StatusUnauthorized, "auth.session_required"
			if expectation, ok := special[route.method+" "+route.path]; ok {
				wantStatus, wantCode = expectation.status, expectation.code
				if route.path == "/api/events" {
					req.Header.Set("Origin", server.URL)
				}
			}
			response, err := server.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			requireOutcome(t, response, wantCode, "null")
			if response.StatusCode != wantStatus ||
				response.Header.Get("X-Content-Type-Options") != "nosniff" ||
				response.Header.Get("Referrer-Policy") != "no-referrer" ||
				response.Header.Get("Content-Security-Policy") == "" {
				t.Fatalf("protected route contract status=%d headers=%v", response.StatusCode, response.Header)
			}
		})
	}
}
