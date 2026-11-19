package lib

import (
	"encoding/json"
	"io"
	"net/http"
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
