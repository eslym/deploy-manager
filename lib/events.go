package lib

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

const (
	eventQueueSize   = 64
	maxEventFrame    = 16 << 10
	maxEventInput    = 1 << 10
	maxAttemptPeers  = 4096
	jobEventInterval = 250 * time.Millisecond
)

type eventResource struct {
	Kind         string `json:"kind"`
	ProjectID    string `json:"projectId,omitempty"`
	DeploymentID string `json:"deploymentId,omitempty"`
}

type eventFrame struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data"`
}

type eventInvalidation struct {
	Epoch     string          `json:"epoch"`
	Sequence  uint64          `json:"sequence"`
	Resources []eventResource `json:"resources"`
}

type eventHub struct {
	app         *App
	mu          sync.Mutex
	broadcastMu sync.Mutex
	epoch       string
	sequence    uint64
	conns       map[*eventConn]struct{}
	attempts    map[string][]time.Time
	jobLast     map[string]time.Time
	jobPending  map[string][]eventResource
	jobTimers   map[string]*time.Timer
	closed      bool
	stop        chan struct{}
	wg          sync.WaitGroup
}

type eventConn struct {
	hub      *eventHub
	conn     *websocket.Conn
	session  [32]byte
	queue    chan []byte
	done     chan struct{}
	once     sync.Once
	lastPong *atomic.Int64
}

func newEventHub(app *App) *eventHub {
	var epoch [16]byte
	if _, err := rand.Read(epoch[:]); err != nil {
		panic("unable to initialize event epoch")
	}
	h := &eventHub{
		app: app, epoch: base64.RawURLEncoding.EncodeToString(epoch[:]),
		conns: make(map[*eventConn]struct{}), attempts: make(map[string][]time.Time),
		jobLast: make(map[string]time.Time), jobPending: make(map[string][]eventResource),
		jobTimers: make(map[string]*time.Timer),
		stop:      make(chan struct{}),
	}
	go h.sweep()
	return h
}

func (h *eventHub) serveHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeOutcomeError(w, http.StatusMethodNotAllowed, "request.method_not_allowed", "Method not allowed.")
		return
	}
	if !isWebSocketHandshake(r) {
		writeOutcomeError(w, http.StatusBadRequest, "request.invalid", "Check the request and try again.")
		return
	}
	if !h.allowAttempt(peerIP(r)) {
		writeOutcomeError(w, http.StatusTooManyRequests, "request.rate_limited", "Too many requests. Try again shortly.")
		return
	}
	if !h.allowedOrigin(r) {
		writeOutcomeError(w, http.StatusForbidden, "request.origin_rejected", "Request origin rejected.")
		return
	}
	if !h.app.authSecure(r) {
		writeOutcomeError(w, http.StatusUpgradeRequired, "request.https_required", "HTTPS is required.")
		return
	}
	digest, ok := h.app.session(r)
	if !ok {
		writeOutcomeError(w, http.StatusUnauthorized, "auth.session_required", "Sign in to continue.")
		return
	}
	h.mu.Lock()
	count := 0
	sessionCount := 0
	for c := range h.conns {
		count++
		if c.session == digest {
			sessionCount++
		}
	}
	if h.closed || count >= 64 || sessionCount >= 8 {
		h.mu.Unlock()
		writeOutcomeError(w, http.StatusTooManyRequests, "request.rate_limited", "Too many requests. Try again shortly.")
		return
	}
	ready, err := json.Marshal(eventFrame{
		Code: "events.ready", Message: "Live updates connected.",
		Data: map[string]any{"epoch": h.epoch, "sequence": h.sequence},
	})
	if err != nil || len(ready) > maxEventFrame {
		h.mu.Unlock()
		writeOutcomeError(w, http.StatusInternalServerError, "service.internal_error", "Operation could not be completed.")
		return
	}
	requestOrigin, _ := url.Parse(r.Header.Get("Origin"))
	// The connection is inserted before ready is written, so committed updates
	// cannot occur in the registration/ready gap.
	lastPong := &atomic.Int64{}
	lastPong.Store(time.Now().UnixNano())
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		OriginPatterns:  []string{requestOrigin.Scheme + "://" + requestOrigin.Host},
		CompressionMode: websocket.CompressionDisabled,
		OnPongReceived: func(context.Context, []byte) {
			lastPong.Store(time.Now().UnixNano())
		},
	})
	if err != nil {
		h.mu.Unlock()
		return
	}
	c := &eventConn{hub: h, conn: ws, session: digest, queue: make(chan []byte, eventQueueSize), done: make(chan struct{}), lastPong: lastPong}
	h.conns[c] = struct{}{}
	c.queue <- ready
	h.wg.Add(3)
	defer h.wg.Done()
	h.mu.Unlock()
	go c.writeLoop()
	go c.livenessLoop()
	c.readLoop()
}

func (h *eventHub) methodNotAllowed(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Allow", http.MethodGet)
	writeOutcomeError(w, http.StatusMethodNotAllowed, "request.method_not_allowed", "Method not allowed.")
}

func isWebSocketHandshake(r *http.Request) bool {
	if r.URL.RawQuery != "" ||
		!strings.EqualFold(strings.TrimSpace(r.Header.Get("Upgrade")), "websocket") ||
		r.Header.Get("Sec-WebSocket-Version") != "13" ||
		r.Header.Get("Sec-WebSocket-Protocol") != "" {
		return false
	}
	connectionUpgrade := false
	for _, value := range r.Header.Values("Connection") {
		for _, token := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(token), "upgrade") {
				connectionUpgrade = true
			}
		}
	}
	if !connectionUpgrade {
		return false
	}
	decoded, err := base64.StdEncoding.DecodeString(r.Header.Get("Sec-WebSocket-Key"))
	return err == nil && len(decoded) == 16
}

func (h *eventHub) allowedOrigin(r *http.Request) bool {
	if h.app.cfg.CanonicalOrigin == "" {
		return false
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return false
	}
	canonical, err := url.Parse(h.app.cfg.CanonicalOrigin)
	if err != nil || canonical.User != nil || canonical.Path != "" || canonical.RawQuery != "" || canonical.Fragment != "" {
		return false
	}
	parsed, err := url.Parse(origin)
	if err != nil || parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return false
	}
	return sameOrigin(canonical, parsed)
}

func peerIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (h *eventHub) allowAttempt(peer string) bool {
	now := h.app.cfg.Now()
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return false
	}
	if len(h.attempts) >= maxAttemptPeers {
		for key, attempts := range h.attempts {
			if len(attempts) == 0 || now.Sub(attempts[len(attempts)-1]) >= time.Minute {
				delete(h.attempts, key)
			}
		}
	}
	if _, exists := h.attempts[peer]; !exists && len(h.attempts) >= maxAttemptPeers {
		return false
	}
	recent := h.attempts[peer][:0]
	for _, at := range h.attempts[peer] {
		if now.Sub(at) < time.Minute {
			recent = append(recent, at)
		}
	}
	if len(recent) >= 20 {
		h.attempts[peer] = recent
		return false
	}
	h.attempts[peer] = append(recent, now)
	return true
}

func (c *eventConn) enqueue(frame []byte) bool {
	select {
	case <-c.done:
		return false
	case c.queue <- frame:
		return true
	default:
		return false
	}
}

func (c *eventConn) writeLoop() {
	defer c.hub.wg.Done()
	ticker := time.NewTicker(25 * time.Second)
	defer ticker.Stop()
	pingResult := make(chan error, 1)
	pingInFlight := false
	for {
		select {
		case <-c.done:
			return
		case frame := <-c.queue:
			if !c.hub.authorized(c.session) {
				c.close(4401, "Sign in again.")
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			err := c.conn.Write(ctx, websocket.MessageText, frame)
			cancel()
			if err != nil {
				c.close(websocket.StatusTryAgainLater, "Live updates are unavailable.")
				return
			}
		case <-ticker.C:
			if !c.hub.authorized(c.session) {
				c.close(4401, "Sign in again.")
				return
			}
			if !pingInFlight {
				pingInFlight = true
				go func() {
					ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					defer cancel()
					pingResult <- c.conn.Ping(ctx)
				}()
			}
		case err := <-pingResult:
			pingInFlight = false
			if err != nil {
				c.close(websocket.StatusTryAgainLater, "Live updates timed out.")
				return
			}
		}
	}
}

func (c *eventConn) livenessLoop() {
	defer c.hub.wg.Done()
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-c.done:
			return
		case <-ticker.C:
			if time.Since(time.Unix(0, c.lastPong.Load())) >= 60*time.Second {
				c.close(websocket.StatusTryAgainLater, "Live updates timed out.")
				return
			}
		}
	}
}

func (c *eventConn) readLoop() {
	defer c.close(websocket.StatusNormalClosure, "")
	c.conn.SetReadLimit(maxEventInput + 1)
	for {
		_, reader, err := c.conn.Reader(context.Background())
		if err != nil {
			if errors.Is(err, websocket.ErrMessageTooBig) {
				c.close(websocket.StatusPolicyViolation, "Client messages are not supported.")
			}
			return
		}
		payload, readErr := io.ReadAll(io.LimitReader(reader, maxEventInput+1))
		if len(payload) > maxEventInput || readErr == nil || len(payload) > 0 {
			c.close(websocket.StatusPolicyViolation, "Client messages are not supported.")
			return
		}
		if readErr != nil {
			if errors.Is(readErr, websocket.ErrMessageTooBig) {
				c.close(websocket.StatusPolicyViolation, "Client messages are not supported.")
			}
			return
		}
	}
}

func (c *eventConn) close(code websocket.StatusCode, reason string) {
	c.once.Do(func() {
		close(c.done)
		c.hub.mu.Lock()
		delete(c.hub.conns, c)
		c.hub.mu.Unlock()
		_ = c.conn.Close(code, reason)
	})
}

func (h *eventHub) authorized(digest [32]byte) bool {
	h.app.mu.Lock()
	defer h.app.mu.Unlock()
	if h.app.refreshCredentialGenerationLocked() != nil {
		return false
	}
	state, ok := h.app.sessions[digest]
	return ok && state.generation == h.app.credentialGeneration && state.expires.After(h.app.cfg.Now())
}

func (h *eventHub) invalidate(resources ...eventResource) {
	if len(resources) == 0 {
		return
	}
	h.broadcastMu.Lock()
	defer h.broadcastMu.Unlock()
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	h.sequence++
	frame, err := json.Marshal(eventFrame{
		Code: "events.invalidate", Message: "Dashboard data changed.",
		Data: eventInvalidation{Epoch: h.epoch, Sequence: h.sequence, Resources: resources},
	})
	if err != nil || len(frame) > maxEventFrame {
		frame, _ = json.Marshal(eventFrame{
			Code: "events.invalidate", Message: "Dashboard data changed.",
			Data: eventInvalidation{Epoch: h.epoch, Sequence: h.sequence, Resources: []eventResource{{Kind: "projects"}, {Kind: "deployments"}}},
		})
	}
	connections := make([]*eventConn, 0, len(h.conns))
	for c := range h.conns {
		connections = append(connections, c)
	}
	h.mu.Unlock()
	for _, c := range connections {
		if !h.authorized(c.session) {
			c.close(4401, "Sign in again.")
			continue
		}
		if !c.enqueue(frame) {
			c.close(websocket.StatusTryAgainLater, "Live updates are busy.")
		}
	}
}

func (a *App) invalidateJob(deploymentID string) {
	if a.events != nil {
		a.events.invalidateJob(deploymentID)
	}
}

func (h *eventHub) invalidateJob(deploymentID string) {
	resources := []eventResource{
		{Kind: "jobs", DeploymentID: deploymentID},
		{Kind: "deployments", DeploymentID: deploymentID},
	}
	now := time.Now()
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	if _, exists := h.jobLast[deploymentID]; !exists && len(h.jobLast) >= maxAttemptPeers {
		h.mu.Unlock()
		h.invalidate(resources...)
		return
	}
	if last, ok := h.jobLast[deploymentID]; !ok || now.Sub(last) >= jobEventInterval {
		h.jobLast[deploymentID] = now
		h.mu.Unlock()
		h.invalidate(resources...)
		return
	}
	h.jobPending[deploymentID] = resources
	if h.jobTimers[deploymentID] == nil {
		delay := jobEventInterval - now.Sub(h.jobLast[deploymentID])
		h.jobTimers[deploymentID] = time.AfterFunc(delay, func() {
			h.flushJobInvalidation(deploymentID)
		})
	}
	h.mu.Unlock()
}

func (h *eventHub) flushJobInvalidation(deploymentID string) {
	h.mu.Lock()
	resources := h.jobPending[deploymentID]
	delete(h.jobPending, deploymentID)
	delete(h.jobTimers, deploymentID)
	if len(resources) == 0 || h.closed {
		h.mu.Unlock()
		return
	}
	h.jobLast[deploymentID] = time.Now()
	h.mu.Unlock()
	h.invalidate(resources...)
}

func (a *App) invalidateDeployment(deploymentID string) {
	if a.events != nil {
		a.events.invalidate(eventResource{Kind: "deployments", DeploymentID: deploymentID})
	}
}

func (h *eventHub) revoke(digest [32]byte) {
	h.mu.Lock()
	var affected []*eventConn
	for c := range h.conns {
		if c.session == digest {
			affected = append(affected, c)
		}
	}
	h.mu.Unlock()
	for _, c := range affected {
		c.close(4401, "Sign in again.")
	}
}

func (h *eventHub) revokeAll() {
	h.mu.Lock()
	connections := make([]*eventConn, 0, len(h.conns))
	for c := range h.conns {
		connections = append(connections, c)
	}
	h.mu.Unlock()
	for _, c := range connections {
		c.close(4401, "Sign in again.")
	}
}

func (h *eventHub) sweep() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-h.stop:
			return
		case <-ticker.C:
			h.mu.Lock()
			connections := make([]*eventConn, 0, len(h.conns))
			for c := range h.conns {
				connections = append(connections, c)
			}
			h.mu.Unlock()
			for _, c := range connections {
				if !h.authorized(c.session) {
					c.close(4401, "Sign in again.")
				}
			}
		}
	}
}

func (h *eventHub) close() {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	h.closed = true
	close(h.stop)
	for _, timer := range h.jobTimers {
		timer.Stop()
	}
	clear(h.jobTimers)
	clear(h.jobPending)
	clear(h.jobLast)
	connections := make([]*eventConn, 0, len(h.conns))
	for c := range h.conns {
		connections = append(connections, c)
	}
	h.mu.Unlock()
	for _, c := range connections {
		c.close(websocket.StatusGoingAway, "Server shutting down.")
	}
	h.wg.Wait()
}
