package lib

import (
	"encoding/json"
	"net/http"
)

type httpOutcome struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data"`
}

func writeOutcome(w http.ResponseWriter, status int, code, message string, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(httpOutcome{Code: code, Message: message, Data: data})
}

func writeOutcomeError(w http.ResponseWriter, status int, code, message string) {
	writeOutcome(w, status, code, message, nil)
}
