package lib

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

func newEventsTestServer(t *testing.T) (*App, *httptest.Server, string) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	origin := "http://" + listener.Addr().String()
	app, err := New(Config{
		DatabasePath:      filepath.Join(t.TempDir(), "state.db"),
		OPAQUEKeyPath:     filepath.Join(t.TempDir(), "opaque.key"),
		CanonicalOrigin:   origin,
		AllowHTTPOrigin:   true,
		InsecureLocalHTTP: false,
	})
	if err != nil {
		_ = listener.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.Close() })
	server := httptest.NewUnstartedServer(app.Handler())
	server.Listener = listener
	server.Start()
	t.Cleanup(server.Close)
	return app, server, origin
}

func newSecureEventsTestServer(t *testing.T) (*App, *httptest.Server, string) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	origin := "http://" + listener.Addr().String()
	app, err := New(Config{
		DatabasePath:    filepath.Join(t.TempDir(), "state.db"),
		OPAQUEKeyPath:   filepath.Join(t.TempDir(), "opaque.key"),
		CanonicalOrigin: origin,
		AllowHTTPOrigin: true,
	})
	if err != nil {
		_ = listener.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.Close() })
	server := httptest.NewUnstartedServer(app.Handler())
	server.Listener = listener
	server.StartTLS()
	t.Cleanup(server.Close)
	return app, server, origin
}

func eventsClient(t *testing.T, serverURL, origin, cookie string) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	header := http.Header{"Origin": []string{origin}, "Cookie": []string{cookie}}
	socketURL := strings.Replace(serverURL, "https://", "wss://", 1)
	socketURL = strings.Replace(socketURL, "http://", "ws://", 1)
	return websocket.Dial(context.Background(), socketURL+"/api/events", &websocket.DialOptions{HTTPHeader: header})
}

func TestEventsRequiresAuthenticatedAllowedOriginBeforeUpgrade(t *testing.T) {
	_, server, origin := newEventsTestServer(t)

	for _, test := range []struct {
		name   string
		origin string
		cookie string
	}{
		{name: "missing session", origin: origin},
		{name: "missing origin", cookie: sessionCookie + "=ignored"},
		{name: "foreign origin", origin: "https://attacker.example", cookie: sessionCookie + "=ignored"},
	} {
		t.Run(test.name, func(t *testing.T) {
			conn, response, err := eventsClient(t, server.URL, test.origin, test.cookie)
			if conn != nil {
				_ = conn.Close(websocket.StatusNormalClosure, "")
				t.Fatal("unexpected WebSocket connection")
			}
			if err == nil || response == nil || response.StatusCode != http.StatusUnauthorized && response.StatusCode != http.StatusForbidden {
				t.Fatalf("expected pre-upgrade rejection, response=%v err=%v", response, err)
			}
		})
	}
}

func TestEventsRejectedHandshakeUsesApplicationEnvelope(t *testing.T) {
	_, server, origin := newEventsTestServer(t)
	request, err := http.NewRequest(http.MethodGet, server.URL+"/api/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Origin", origin)
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", response.StatusCode)
	}
	var result struct {
		Code    string `json:"code"`
		Message string `json:"message"`
		Data    any    `json:"data"`
	}
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if result.Code != "request.invalid" || result.Message == "" || result.Data != nil {
		t.Fatalf("unexpected handshake rejection: %+v", result)
	}
}

func TestEventsRejectsApplicationMethodsWithAllowHeader(t *testing.T) {
	_, server, _ := newEventsTestServer(t)
	request, err := http.NewRequest(http.MethodPost, server.URL+"/api/events", strings.NewReader(""))
	if err != nil {
		t.Fatal(err)
	}
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusMethodNotAllowed || response.Header.Get("Allow") != http.MethodGet {
		t.Fatalf("unexpected method rejection: status=%d Allow=%q", response.StatusCode, response.Header.Get("Allow"))
	}
	var result struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if result.Code != "request.method_not_allowed" {
		t.Fatalf("wrong method-rejection code: %+v", result)
	}
}

func TestEventsHandshakeSendsReadyAfterAuthentication(t *testing.T) {
	app, server, origin := newEventsTestServer(t)
	token := make([]byte, 32)
	for i := range token {
		token[i] = byte(i + 1)
	}
	digest := sha256.Sum256(token)
	app.mu.Lock()
	app.sessions[digest] = sessionState{expires: time.Now().Add(time.Hour), generation: app.credentialGeneration}
	app.mu.Unlock()

	cookie := app.cookieName() + "=" + base64.RawURLEncoding.EncodeToString(token)
	conn, response, err := eventsClient(t, server.URL, origin, cookie)
	if err != nil {
		t.Fatalf("dial events: status=%v err=%v", response, err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")

	var frame struct {
		Code string `json:"code"`
		Data struct {
			Epoch    string `json:"epoch"`
			Sequence uint64 `json:"sequence"`
		} `json:"data"`
	}
	if err := wsjson.Read(context.Background(), conn, &frame); err != nil {
		t.Fatal(err)
	}
	if frame.Code != "events.ready" || frame.Data.Epoch == "" {
		t.Fatalf("unexpected ready frame: %+v", frame)
	}
	encoded, err := json.Marshal(frame)
	if err != nil || len(encoded) > 16<<10 {
		t.Fatalf("ready frame exceeds bound: len=%d err=%v", len(encoded), err)
	}
}

func TestEventsEnforcesPerSessionConnectionLimit(t *testing.T) {
	app, server, origin := newEventsTestServer(t)
	token := make([]byte, 32)
	for i := range token {
		token[i] = byte(i + 1)
	}
	digest := sha256.Sum256(token)
	app.mu.Lock()
	app.sessions[digest] = sessionState{expires: time.Now().Add(time.Hour), generation: app.credentialGeneration}
	app.mu.Unlock()
	cookie := app.cookieName() + "=" + base64.RawURLEncoding.EncodeToString(token)
	connections := make([]*websocket.Conn, 0, 8)
	t.Cleanup(func() {
		for _, conn := range connections {
			_ = conn.Close(websocket.StatusNormalClosure, "")
		}
	})
	for range 8 {
		conn, response, err := eventsClient(t, server.URL, origin, cookie)
		if err != nil {
			t.Fatalf("connection within limit rejected: response=%v err=%v", response, err)
		}
		connections = append(connections, conn)
		var ready map[string]any
		if err := wsjson.Read(context.Background(), conn, &ready); err != nil {
			t.Fatal(err)
		}
	}
	conn, response, err := eventsClient(t, server.URL, origin, cookie)
	if conn != nil {
		_ = conn.Close(websocket.StatusNormalClosure, "")
		t.Fatal("ninth session connection unexpectedly upgraded")
	}
	if err == nil || response == nil || response.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("ninth connection response=%v err=%v, want 429", response, err)
	}
}

func TestEventsConnectionAttemptsUseSeparatePeerBudget(t *testing.T) {
	_, server, _ := newEventsTestServer(t)
	for attempt := 1; attempt <= 21; attempt++ {
		_, response, err := eventsClient(t, server.URL, "", "")
		if err == nil || response == nil {
			t.Fatalf("attempt %d unexpectedly upgraded", attempt)
		}
		want := http.StatusForbidden
		if attempt == 21 {
			want = http.StatusTooManyRequests
		}
		if response.StatusCode != want {
			t.Fatalf("attempt %d returned %d, want %d", attempt, response.StatusCode, want)
		}
	}
}

func TestDeploymentQueueCommitInvalidatesAnOpenSocket(t *testing.T) {
	app, server, origin := newEventsTestServer(t)
	token := make([]byte, 32)
	for i := range token {
		token[i] = byte(i + 1)
	}
	digest := sha256.Sum256(token)
	app.mu.Lock()
	app.sessions[digest] = sessionState{expires: time.Now().Add(time.Hour), generation: app.credentialGeneration}
	app.mu.Unlock()
	now := time.Now().UnixNano()
	if _, err := app.store.Project.Create().SetID("project-1").SetName("Project").SetRemoteURL("https://git.example/repo").SetCreatedAt(now).Save(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := app.store.Deployment.Create().SetID("deployment-1").SetProjectID("project-1").SetName("Site").SetCommitID(strings.Repeat("a", 40)).SetPublicationName("site").SetGeneration(1).SetStatus("live").SetCreatedAt(now).SetUpdatedAt(now).SetTargetKind("commit").Save(context.Background()); err != nil {
		t.Fatal(err)
	}
	cookie := app.cookieName() + "=" + base64.RawURLEncoding.EncodeToString(token)
	conn, response, err := eventsClient(t, server.URL, origin, cookie)
	if err != nil {
		t.Fatalf("dial events: response=%v err=%v", response, err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")
	var ready map[string]any
	if err := wsjson.Read(context.Background(), conn, &ready); err != nil {
		t.Fatal(err)
	}

	request, err := http.NewRequest(http.MethodPost, server.URL+"/api/deployments/deployment-1/deploy", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Origin", origin)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Cookie", cookie)
	result, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	result.Body.Close()
	if result.StatusCode != http.StatusAccepted {
		t.Fatalf("deployment not accepted: %s", result.Status)
	}

	var frame struct {
		Code string `json:"code"`
		Data struct {
			Sequence  uint64 `json:"sequence"`
			Resources []struct {
				Kind         string `json:"kind"`
				ProjectID    string `json:"projectId"`
				DeploymentID string `json:"deploymentId"`
			} `json:"resources"`
		} `json:"data"`
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := wsjson.Read(ctx, conn, &frame); err != nil {
		t.Fatal(err)
	}
	if frame.Code != "events.invalidate" || frame.Data.Sequence == 0 || len(frame.Data.Resources) < 2 {
		t.Fatalf("missing committed deployment/job invalidation: %+v", frame)
	}
	kinds := map[string]bool{}
	for _, resource := range frame.Data.Resources {
		switch resource.Kind {
		case "jobs", "deployments":
			kinds[resource.Kind] = resource.DeploymentID == "deployment-1"
		case "refs":
			kinds[resource.Kind] = resource.ProjectID == "project-1"
		case "projects":
			kinds[resource.Kind] = true
		}
	}
	if !kinds["jobs"] || !kinds["deployments"] || !kinds["projects"] || !kinds["refs"] {
		t.Fatalf("invalidation omitted dependent project, ref, deployment, or job reads: %+v", frame.Data.Resources)
	}
	read, err := http.NewRequest(http.MethodGet, server.URL+"/api/deployments/deployment-1/jobs", nil)
	if err != nil {
		t.Fatal(err)
	}
	read.Header.Set("Cookie", cookie)
	refetched, err := http.DefaultClient.Do(read)
	if err != nil {
		t.Fatal(err)
	}
	defer refetched.Body.Close()
	var jobsOutcome struct {
		Code string          `json:"code"`
		Data []deploymentJob `json:"data"`
	}
	if err := json.NewDecoder(refetched.Body).Decode(&jobsOutcome); err != nil {
		t.Fatal(err)
	}
	if refetched.StatusCode != http.StatusOK || jobsOutcome.Code != "job.listed" || len(jobsOutcome.Data) != 1 {
		t.Fatalf("HTTP refetch did not observe committed job: status=%d outcome=%+v", refetched.StatusCode, jobsOutcome)
	}
}

func TestJobRetentionInvalidatesSocketAfterCommittedPruning(t *testing.T) {
	app, server, origin := newEventsTestServer(t)
	token := make([]byte, 32)
	for i := range token {
		token[i] = byte(i + 1)
	}
	digest := sha256.Sum256(token)
	app.mu.Lock()
	app.sessions[digest] = sessionState{expires: time.Now().Add(time.Hour), generation: app.credentialGeneration}
	app.mu.Unlock()

	now := time.Now()
	if _, err := app.store.Project.Create().SetID("project-1").SetName("Project").SetRemoteURL("https://git.example/repo").SetCreatedAt(now.UnixNano()).Save(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := app.store.Deployment.Create().SetID("deployment-1").SetProjectID("project-1").SetName("Site").SetCommitID(strings.Repeat("a", 40)).SetPublicationName("site").SetGeneration(1).SetStatus("live").SetCreatedAt(now.UnixNano()).SetUpdatedAt(now.UnixNano()).SetTargetKind("commit").Save(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := app.store.DeploymentJob.Create().SetID("job-expired").SetDeploymentID("deployment-1").SetCommitID(strings.Repeat("a", 40)).SetGeneration(1).SetStatus("succeeded").SetLog("Published").SetCreatedAt(now.Add(-2 * time.Hour).UnixNano()).SetUpdatedAt(now.Add(-2 * time.Hour).UnixNano()).Save(context.Background()); err != nil {
		t.Fatal(err)
	}
	app.cfg.JobRetention = time.Hour
	cookie := app.cookieName() + "=" + base64.RawURLEncoding.EncodeToString(token)
	conn, response, err := eventsClient(t, server.URL, origin, cookie)
	if err != nil {
		t.Fatalf("dial events: response=%v err=%v", response, err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")
	var ready map[string]any
	if err := wsjson.Read(context.Background(), conn, &ready); err != nil {
		t.Fatal(err)
	}

	if err := app.cleanupPublications(true); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var frame struct {
		Code string `json:"code"`
		Data struct {
			Resources []eventResource `json:"resources"`
		} `json:"data"`
	}
	if err := wsjson.Read(ctx, conn, &frame); err != nil {
		t.Fatalf("read retention invalidation: %v", err)
	}
	if frame.Code != "events.invalidate" {
		t.Fatalf("unexpected event frame: %+v", frame)
	}
	var invalidatedJobs bool
	for _, resource := range frame.Data.Resources {
		if resource.Kind == "jobs" && resource.DeploymentID == "deployment-1" {
			invalidatedJobs = true
		}
	}
	if !invalidatedJobs {
		t.Fatalf("retention event did not identify the affected jobs: %+v", frame.Data.Resources)
	}

	request, err := http.NewRequest(http.MethodGet, server.URL+"/api/deployments/deployment-1/jobs", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.AddCookie(&http.Cookie{Name: app.cookieName(), Value: base64.RawURLEncoding.EncodeToString(token)})
	responseHTTP, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer responseHTTP.Body.Close()
	var outcome struct {
		Data []deploymentJob `json:"data"`
	}
	if err := json.NewDecoder(responseHTTP.Body).Decode(&outcome); err != nil {
		t.Fatal(err)
	}
	if responseHTTP.StatusCode != http.StatusOK || len(outcome.Data) != 0 {
		t.Fatalf("jobs read after retention = status %d, data %+v", responseHTTP.StatusCode, outcome.Data)
	}
}

func TestConcurrentCommittedInvalidationsPreserveSocketSequenceOrder(t *testing.T) {
	app, server, origin := newEventsTestServer(t)
	token := make([]byte, 32)
	for i := range token {
		token[i] = byte(i + 1)
	}
	digest := sha256.Sum256(token)
	app.mu.Lock()
	app.sessions[digest] = sessionState{expires: time.Now().Add(time.Hour), generation: app.credentialGeneration}
	app.mu.Unlock()
	now := time.Now().UnixNano()
	if _, err := app.store.Project.Create().SetID("project-1").SetName("Project").SetRemoteURL("https://git.example/repo").SetCreatedAt(now).Save(context.Background()); err != nil {
		t.Fatal(err)
	}
	deploymentIDs := []string{"deployment-1", "deployment-2"}
	for _, id := range deploymentIDs {
		if _, err := app.store.Deployment.Create().SetID(id).SetProjectID("project-1").SetName(id).SetCommitID(strings.Repeat("a", 40)).SetPublicationName(id).SetGeneration(1).SetStatus("live").SetCreatedAt(now).SetUpdatedAt(now).SetTargetKind("commit").Save(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	cookie := app.cookieName() + "=" + base64.RawURLEncoding.EncodeToString(token)
	conn, response, err := eventsClient(t, server.URL, origin, cookie)
	if err != nil {
		t.Fatalf("dial events: response=%v err=%v", response, err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")
	var ready map[string]any
	if err := wsjson.Read(context.Background(), conn, &ready); err != nil {
		t.Fatal(err)
	}

	start := make(chan struct{})
	results := make(chan error, len(deploymentIDs))
	var group sync.WaitGroup
	for _, id := range deploymentIDs {
		group.Add(1)
		go func(id string) {
			defer group.Done()
			<-start
			request, requestErr := http.NewRequest(http.MethodPost, server.URL+"/api/deployments/"+id+"/deploy", strings.NewReader("{}"))
			if requestErr != nil {
				results <- requestErr
				return
			}
			request.Header.Set("Origin", origin)
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Cookie", cookie)
			result, requestErr := http.DefaultClient.Do(request)
			if requestErr != nil {
				results <- requestErr
				return
			}
			result.Body.Close()
			if result.StatusCode != http.StatusAccepted {
				results <- fmt.Errorf("deployment action status = %d", result.StatusCode)
				return
			}
			results <- nil
		}(id)
	}
	close(start)
	group.Wait()
	close(results)
	for requestErr := range results {
		if requestErr != nil {
			t.Fatal(requestErr)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var previous uint64
	received := 0
	for received < len(deploymentIDs) {
		var frame struct {
			Code string `json:"code"`
			Data struct {
				Sequence uint64 `json:"sequence"`
			} `json:"data"`
		}
		if err := wsjson.Read(ctx, conn, &frame); err != nil {
			t.Fatal(err)
		}
		if frame.Code != "events.invalidate" {
			continue
		}
		if frame.Data.Sequence <= previous {
			t.Fatalf("invalidation sequence decreased: got %d after %d", frame.Data.Sequence, previous)
		}
		previous = frame.Data.Sequence
		received++
	}
}

func TestSupersededWorkerJobInvalidatesOpenSocket(t *testing.T) {
	app, server, origin := newEventsTestServer(t)
	token := make([]byte, 32)
	for i := range token {
		token[i] = byte(i + 1)
	}
	digest := sha256.Sum256(token)
	app.mu.Lock()
	app.sessions[digest] = sessionState{expires: time.Now().Add(time.Hour), generation: app.credentialGeneration}
	app.mu.Unlock()
	now := time.Now().UnixNano()
	if _, err := app.store.Project.Create().SetID("project-1").SetName("Project").SetRemoteURL("https://git.example/repo").SetCreatedAt(now).Save(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := app.store.Deployment.Create().SetID("deployment-1").SetProjectID("project-1").SetName("Site").SetCommitID(strings.Repeat("a", 40)).SetPublicationName("site").SetGeneration(2).SetStatus("queued").SetCreatedAt(now).SetUpdatedAt(now).SetTargetKind("commit").Save(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := app.store.DeploymentJob.Create().SetID("job-1").SetDeploymentID("deployment-1").SetCommitID(strings.Repeat("a", 40)).SetGeneration(1).SetStatus("queued").SetLog("Publication queued").SetCreatedAt(now).SetUpdatedAt(now).Save(context.Background()); err != nil {
		t.Fatal(err)
	}
	cookie := app.cookieName() + "=" + base64.RawURLEncoding.EncodeToString(token)
	conn, response, err := eventsClient(t, server.URL, origin, cookie)
	if err != nil {
		t.Fatalf("dial events: response=%v err=%v", response, err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")
	var ready map[string]any
	if err := wsjson.Read(context.Background(), conn, &ready); err != nil {
		t.Fatal(err)
	}

	app.runLockedDeploymentJob("job-1")

	var frame struct {
		Code string `json:"code"`
		Data struct {
			Resources []eventResource `json:"resources"`
		} `json:"data"`
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := wsjson.Read(ctx, conn, &frame); err != nil {
		t.Fatal(err)
	}
	if frame.Code != "events.invalidate" || len(frame.Data.Resources) == 0 || frame.Data.Resources[0].Kind != "jobs" {
		t.Fatalf("missing terminal job invalidation: %+v", frame)
	}
	job, err := app.store.DeploymentJob.Get(context.Background(), "job-1")
	if err != nil {
		t.Fatal(err)
	}
	if job.Status != "superseded" {
		t.Fatalf("worker job status = %q, want superseded", job.Status)
	}
}

func TestWebhookQueuedJobInvalidatesOpenSocket(t *testing.T) {
	app, server, origin := newSecureEventsTestServer(t)
	token := make([]byte, 32)
	for i := range token {
		token[i] = byte(i + 1)
	}
	digest := sha256.Sum256(token)
	app.mu.Lock()
	app.sessions[digest] = sessionState{expires: time.Now().Add(time.Hour), generation: app.credentialGeneration}
	app.mu.Unlock()
	now := time.Now().UnixNano()
	projectID, err := randomID()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.store.Project.Create().SetID(projectID).SetName("Project").SetRemoteURL("https://git.example/repo").SetCreatedAt(now).Save(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := app.store.Deployment.Create().SetID("deployment-1").SetProjectID(projectID).SetName("Site").SetCommitID(strings.Repeat("a", 40)).SetPublicationName("site").SetGeneration(1).SetStatus("live").SetCreatedAt(now).SetUpdatedAt(now).SetTargetKind("branch").SetTargetName("main").Save(context.Background()); err != nil {
		t.Fatal(err)
	}
	secret := []byte("dedicated webhook signing secret for test")
	sealed, err := app.sealWebhookSecret(projectID, secret)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.Exec(`INSERT INTO project_webhooks(project_id,secret,created_at) VALUES(?,?,?)`, projectID, sealed, now); err != nil {
		t.Fatal(err)
	}
	cookie := app.cookieName() + "=" + base64.RawURLEncoding.EncodeToString(token)
	header := http.Header{"Origin": []string{origin}, "Cookie": []string{cookie}}
	conn, response, err := websocket.Dial(context.Background(), strings.Replace(server.URL, "https://", "wss://", 1)+"/api/events", &websocket.DialOptions{
		HTTPHeader: header,
		HTTPClient: server.Client(),
	})
	if err != nil {
		t.Fatalf("dial events: response=%v err=%v", response, err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")
	var ready map[string]any
	if err := wsjson.Read(context.Background(), conn, &ready); err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"ref":"refs/heads/main","after":"` + strings.Repeat("b", 40) + `","deleted":false}`)
	if status := sendGitHubWebhook(server.Client(), server.URL+"/webhooks/github/"+projectID, "delivery-1", "push", body, secret); status != http.StatusOK {
		t.Fatalf("webhook status = %d, want 200", status)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	for {
		var frame struct {
			Code string `json:"code"`
			Data struct {
				Resources []eventResource `json:"resources"`
			} `json:"data"`
		}
		if err := wsjson.Read(ctx, conn, &frame); err != nil {
			t.Fatal(err)
		}
		if frame.Code != "events.invalidate" {
			continue
		}
		for _, resource := range frame.Data.Resources {
			if resource.Kind == "jobs" && resource.DeploymentID == "deployment-1" {
				return
			}
		}
	}
}

func TestLogoutImmediatelyClosesTheSessionSocket(t *testing.T) {
	app, server, origin := newEventsTestServer(t)
	token := make([]byte, 32)
	for i := range token {
		token[i] = byte(i + 1)
	}
	digest := sha256.Sum256(token)
	app.mu.Lock()
	app.sessions[digest] = sessionState{expires: time.Now().Add(time.Hour), generation: app.credentialGeneration}
	app.mu.Unlock()
	cookie := app.cookieName() + "=" + base64.RawURLEncoding.EncodeToString(token)
	conn, response, err := eventsClient(t, server.URL, origin, cookie)
	if err != nil {
		t.Fatalf("dial events: response=%v err=%v", response, err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")
	var ready map[string]any
	if err := wsjson.Read(context.Background(), conn, &ready); err != nil {
		t.Fatal(err)
	}

	request, err := http.NewRequest(http.MethodPost, server.URL+"/api/logout", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Origin", origin)
	request.Header.Set("Cookie", cookie)
	result, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	result.Body.Close()
	if result.StatusCode != http.StatusOK {
		t.Fatalf("logout failed: %s", result.Status)
	}
	_, _, err = conn.Read(context.Background())
	if websocket.CloseStatus(err) != 4401 {
		t.Fatalf("socket close code = %d, error=%v", websocket.CloseStatus(err), err)
	}
}

func TestIdleSocketClosesWhenSessionExpires(t *testing.T) {
	app, server, origin := newEventsTestServer(t)
	token := make([]byte, 32)
	for i := range token {
		token[i] = byte(i + 1)
	}
	digest := sha256.Sum256(token)
	app.mu.Lock()
	app.sessions[digest] = sessionState{expires: time.Now().Add(100 * time.Millisecond), generation: app.credentialGeneration}
	app.mu.Unlock()
	cookie := app.cookieName() + "=" + base64.RawURLEncoding.EncodeToString(token)
	conn, response, err := eventsClient(t, server.URL, origin, cookie)
	if err != nil {
		t.Fatalf("dial events: response=%v err=%v", response, err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")
	var ready map[string]any
	if err := wsjson.Read(context.Background(), conn, &ready); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 7*time.Second)
	defer cancel()
	_, _, err = conn.Read(ctx)
	if websocket.CloseStatus(err) != 4401 {
		t.Fatalf("expired idle session close = %v, want 4401", err)
	}
}

func TestEventsShutdownClosesSockets(t *testing.T) {
	app, server, origin := newEventsTestServer(t)
	token := make([]byte, 32)
	for i := range token {
		token[i] = byte(i + 1)
	}
	digest := sha256.Sum256(token)
	app.mu.Lock()
	app.sessions[digest] = sessionState{expires: time.Now().Add(time.Hour), generation: app.credentialGeneration}
	app.mu.Unlock()
	cookie := app.cookieName() + "=" + base64.RawURLEncoding.EncodeToString(token)
	conn, response, err := eventsClient(t, server.URL, origin, cookie)
	if err != nil {
		t.Fatalf("dial events: response=%v err=%v", response, err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")
	var ready map[string]any
	if err := wsjson.Read(context.Background(), conn, &ready); err != nil {
		t.Fatal(err)
	}
	if err := app.Close(); err != nil {
		t.Fatal(err)
	}
	_, _, err = conn.Read(context.Background())
	if websocket.CloseStatus(err) != websocket.StatusGoingAway {
		t.Fatalf("socket close code = %d, error=%v", websocket.CloseStatus(err), err)
	}
}

func TestReadOnlyEventsSocketRejectsApplicationInput(t *testing.T) {
	app, server, origin := newEventsTestServer(t)
	token := make([]byte, 32)
	for i := range token {
		token[i] = byte(i + 1)
	}
	digest := sha256.Sum256(token)
	app.mu.Lock()
	app.sessions[digest] = sessionState{expires: time.Now().Add(time.Hour), generation: app.credentialGeneration}
	app.mu.Unlock()
	cookie := app.cookieName() + "=" + base64.RawURLEncoding.EncodeToString(token)
	conn, response, err := eventsClient(t, server.URL, origin, cookie)
	if err != nil {
		t.Fatalf("dial events: response=%v err=%v", response, err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")
	var ready map[string]any
	if err := wsjson.Read(context.Background(), conn, &ready); err != nil {
		t.Fatal(err)
	}
	if err := conn.Write(context.Background(), websocket.MessageText, []byte("subscribe")); err != nil {
		t.Fatal(err)
	}
	_, _, err = conn.Read(context.Background())
	if websocket.CloseStatus(err) != websocket.StatusPolicyViolation {
		t.Fatalf("socket close code = %d, error=%v", websocket.CloseStatus(err), err)
	}
}
