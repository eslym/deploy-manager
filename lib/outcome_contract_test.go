package lib

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
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
