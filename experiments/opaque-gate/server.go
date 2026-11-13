package main

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"log"
	"net/http"
	"sync"

	"github.com/bytemare/opaque"
)

type server struct {
	mu       sync.Mutex
	protocol *opaque.Server
	des      *opaque.Deserializer
	record   []byte
	pending  map[string][]byte
}

func main() {
	cfg := opaque.DefaultConfiguration()
	protocol, err := opaque.NewServer(cfg)
	if err != nil {
		log.Fatal(err)
	}
	priv, pub := cfg.KeyGen()
	skm := &opaque.ServerKeyMaterial{PrivateKey: priv, PublicKeyBytes: pub.Encode(), OPRFGlobalSeed: cfg.GenerateOPRFSeed()}
	if err = protocol.SetKeyMaterial(skm); err != nil {
		log.Fatal(err)
	}
	des, err := cfg.Deserializer()
	if err != nil {
		log.Fatal(err)
	}
	s := &server{protocol: protocol, des: des, pending: map[string][]byte{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/opaque.js", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "node_modules/@serenity-kit/opaque/esm/index.js")
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { http.ServeFile(w, r, "index.html") })
	mux.HandleFunc("/register/start", s.registerStart)
	mux.HandleFunc("/register/finish", s.registerFinish)
	mux.HandleFunc("/login/start", s.loginStart)
	mux.HandleFunc("/login/finish", s.loginFinish)
	log.Fatal(http.ListenAndServe("127.0.0.1:4179", mux))
}

type message struct {
	Value string `json:"value"`
	Tx    string `json:"tx"`
}

func decode(r *http.Request) (message, error) {
	var m message
	err := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 65536)).Decode(&m)
	return m, err
}
func reply(w http.ResponseWriter, value string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"value": value})
}
func bad(w http.ResponseWriter) { http.Error(w, "invalid protocol message", http.StatusBadRequest) }

func (s *server) registerStart(w http.ResponseWriter, r *http.Request) {
	m, err := decode(r)
	if err != nil {
		bad(w)
		return
	}
	b, err := base64.RawURLEncoding.DecodeString(m.Value)
	if err != nil {
		bad(w)
		return
	}
	req, err := s.des.RegistrationRequest(b)
	if err != nil {
		bad(w)
		return
	}
	resp, err := s.protocol.RegistrationResponse(req, []byte("gate-user"), nil)
	if err != nil {
		bad(w)
		return
	}
	reply(w, base64.RawURLEncoding.EncodeToString(resp.Serialize()))
}
func (s *server) registerFinish(w http.ResponseWriter, r *http.Request) {
	m, err := decode(r)
	if err != nil {
		bad(w)
		return
	}
	b, err := base64.RawURLEncoding.DecodeString(m.Value)
	if err != nil {
		bad(w)
		return
	}
	if _, err = s.des.RegistrationRecord(b); err != nil {
		bad(w)
		return
	}
	s.mu.Lock()
	s.record = append([]byte(nil), b...)
	s.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}
func (s *server) loginStart(w http.ResponseWriter, r *http.Request) {
	m, err := decode(r)
	if err != nil {
		bad(w)
		return
	}
	b, err := base64.RawURLEncoding.DecodeString(m.Value)
	if err != nil {
		bad(w)
		return
	}
	ke1, err := s.des.KE1(b)
	if err != nil {
		bad(w)
		return
	}
	s.mu.Lock()
	rec := append([]byte(nil), s.record...)
	s.mu.Unlock()
	if len(rec) == 0 {
		http.Error(w, "not enrolled", http.StatusConflict)
		return
	}
	parsed, err := s.des.RegistrationRecord(rec)
	if err != nil {
		bad(w)
		return
	}
	ke2, out, err := s.protocol.GenerateKE2(ke1, &opaque.ClientRecord{RegistrationRecord: parsed, CredentialIdentifier: []byte("gate-user")})
	if err != nil {
		bad(w)
		return
	}
	tx := make([]byte, 18)
	if _, err = rand.Read(tx); err != nil {
		http.Error(w, "internal error", 500)
		return
	}
	id := base64.RawURLEncoding.EncodeToString(tx)
	s.mu.Lock()
	s.pending[id] = append([]byte(nil), out.ClientMAC...)
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"value": base64.RawURLEncoding.EncodeToString(ke2.Serialize()), "tx": id})
}
func (s *server) loginFinish(w http.ResponseWriter, r *http.Request) {
	m, err := decode(r)
	if err != nil {
		bad(w)
		return
	}
	b, err := base64.RawURLEncoding.DecodeString(m.Value)
	if err != nil {
		bad(w)
		return
	}
	ke3, err := s.des.KE3(b)
	if err != nil {
		bad(w)
		return
	}
	s.mu.Lock()
	mac, ok := s.pending[m.Tx]
	delete(s.pending, m.Tx)
	s.mu.Unlock()
	if !ok || s.protocol.LoginFinish(ke3, mac) != nil {
		bad(w)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
