package lib

import (
	"bufio"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestGitHubWebhookValidatesRawSignatureAndDeduplicatesIgnoredDelivery(t *testing.T) {
	app, projectID, secret := webhookTestApp(t, 2048)
	server := httptest.NewTLSServer(app.Handler())
	defer server.Close()
	body := []byte(`{"ref":"refs/heads/unmatched","after":"` + strings.Repeat("a", 40) + `","deleted":false}`)
	var group sync.WaitGroup
	var failures atomic.Int32
	for i := 0; i < 12; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			if response := sendGitHubWebhook(server.Client(), server.URL+"/webhooks/github/"+projectID, "delivery-one", "push", body, secret); response != http.StatusNoContent {
				failures.Add(1)
			}
		}()
	}
	group.Wait()
	if failures.Load() != 0 {
		t.Fatalf("%d concurrent duplicate deliveries were not acknowledged", failures.Load())
	}
	var receipts, jobs int
	if err := app.db.QueryRow(`SELECT count(*) FROM webhook_deliveries WHERE project_id=?`, projectID).Scan(&receipts); err != nil {
		t.Fatal(err)
	}
	if err := app.db.QueryRow(`SELECT count(*) FROM deployment_jobs`).Scan(&jobs); err != nil {
		t.Fatal(err)
	}
	if receipts != 1 || jobs != 0 {
		t.Fatalf("receipts=%d jobs=%d, want one ignored receipt and no jobs", receipts, jobs)
	}
	secondProject, err := randomID()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = app.db.Exec(`INSERT INTO projects(id,name,remote_url,created_at) VALUES(?,?,?,?)`, secondProject, "second fixture", "https://example.invalid/second.git", time.Now().UnixNano()); err != nil {
		t.Fatal(err)
	}
	secondSecret := []byte("different project webhook key")
	sealed, err := app.sealWebhookSecret(secondProject, secondSecret)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = app.db.Exec(`INSERT INTO project_webhooks(project_id,secret,created_at) VALUES(?,?,?)`, secondProject, sealed, time.Now().UnixNano()); err != nil {
		t.Fatal(err)
	}
	if status := sendGitHubWebhook(server.Client(), server.URL+"/webhooks/github/"+secondProject, "delivery-one", "push", body, secondSecret); status != http.StatusNoContent {
		t.Fatalf("same delivery ID in a different project status = %d, want 204", status)
	}
	if status := sendGitHubWebhook(server.Client(), server.URL+"/webhooks/github/"+secondProject, "delivery-one", "push", body, secret); status != http.StatusUnauthorized {
		t.Fatalf("project A secret authenticated project B request: status=%d", status)
	}
	if err = app.db.QueryRow(`SELECT count(*) FROM webhook_deliveries`).Scan(&receipts); err != nil || receipts != 2 {
		t.Fatalf("project-scoped receipts=%d err=%v, want 2", receipts, err)
	}
	if _, err := app.db.Exec(`DELETE FROM webhook_deliveries`); err != nil {
		t.Fatal(err)
	}
	badSignature := sendGitHubWebhook(server.Client(), server.URL+"/webhooks/github/"+projectID, "bad", "push", []byte(`{"ref":"refs/heads/main"}`), []byte("wrong"))
	if badSignature != http.StatusUnauthorized {
		t.Fatalf("bad signature status = %d, want 401", badSignature)
	}
	var count int
	if err := app.db.QueryRow(`SELECT count(*) FROM webhook_deliveries`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("bad signature created receipt: count=%d err=%v", count, err)
	}
}

func TestGitHubWebhookDoesNotAcknowledgeNonDuplicateConstraintFailure(t *testing.T) {
	app, projectID, secret := webhookTestApp(t, 2048)
	defer app.Close()
	if _, err := app.db.Exec(`CREATE TRIGGER reject_webhook_delivery BEFORE INSERT ON webhook_deliveries BEGIN SELECT RAISE(ABORT, 'injected integrity failure'); END`); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(app.Handler())
	defer server.Close()
	body := []byte(`{"ref":"refs/heads/main","after":"` + strings.Repeat("a", 40) + `","deleted":false}`)
	status := sendGitHubWebhook(server.Client(), server.URL+"/webhooks/github/"+projectID, "constraint-failure", "push", body, secret)
	if status != http.StatusServiceUnavailable {
		t.Fatalf("integrity failure status = %d, want 503", status)
	}
	var receipts, jobs int
	if err := app.db.QueryRow(`SELECT count(*) FROM webhook_deliveries`).Scan(&receipts); err != nil {
		t.Fatal(err)
	}
	if err := app.db.QueryRow(`SELECT count(*) FROM deployment_jobs`).Scan(&jobs); err != nil {
		t.Fatal(err)
	}
	if receipts != 0 || jobs != 0 {
		t.Fatalf("integrity failure persisted receipts=%d jobs=%d, want neither", receipts, jobs)
	}
}

func TestGitHubWebhookRejectsOversizedAndMalformedPushBeforeAcceptance(t *testing.T) {
	app, projectID, secret := webhookTestApp(t, 1024)
	server := httptest.NewTLSServer(app.Handler())
	defer server.Close()
	oversized := sendGitHubWebhook(server.Client(), server.URL+"/webhooks/github/"+projectID, "too-large", "push", []byte(strings.Repeat("x", 1025)), secret)
	if oversized != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body status = %d, want 413", oversized)
	}
	malformed := sendGitHubWebhook(server.Client(), server.URL+"/webhooks/github/"+projectID, "malformed", "push", []byte(`{"ref":"refs/tags/v1"}`), secret)
	if malformed != http.StatusBadRequest {
		t.Fatalf("malformed push status = %d, want 400", malformed)
	}
	var count int
	if err := app.db.QueryRow(`SELECT count(*) FROM webhook_deliveries`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("rejected deliveries created receipts: count=%d err=%v", count, err)
	}
}

func TestGitHubWebhookSetupIsProtectedAndRevealsSecretOnlyOnRotation(t *testing.T) {
	app, projectID, _ := webhookTestApp(t, 2048)
	server := httptest.NewTLSServer(app.Handler())
	defer server.Close()
	token := []byte(strings.Repeat("s", 32))
	digest := sha256.Sum256(token)
	app.sessions[digest] = sessionState{expires: time.Now().Add(time.Hour), generation: 0, authenticatedAt: time.Now()}
	cookie := &http.Cookie{Name: sessionCookie, Value: base64.RawURLEncoding.EncodeToString(token), Secure: true, HttpOnly: true}
	urlPath := server.URL + "/api/projects/" + projectID + "/github-webhook"
	request, _ := http.NewRequest(http.MethodGet, urlPath, nil)
	request.AddCookie(cookie)
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	var status map[string]any
	_ = json.NewDecoder(response.Body).Decode(&status)
	response.Body.Close()
	if response.StatusCode != http.StatusOK || status["configured"] != true || status["secret"] != nil {
		t.Fatalf("setup status response = %d %#v", response.StatusCode, status)
	}
	request, _ = http.NewRequest(http.MethodPost, urlPath, strings.NewReader(`{"rotate":true}`))
	request.AddCookie(cookie)
	request.Header.Set("Origin", server.URL)
	request.Header.Set("Content-Type", "application/json")
	response, err = server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	var setup map[string]string
	_ = json.NewDecoder(response.Body).Decode(&setup)
	response.Body.Close()
	if response.StatusCode != http.StatusCreated || len(setup["secret"]) != 64 || setup["url"] != "/webhooks/github/"+projectID {
		t.Fatalf("rotation response = %d %#v", response.StatusCode, setup)
	}
	var stored []byte
	if err := app.db.QueryRow(`SELECT secret FROM project_webhooks WHERE project_id=?`, projectID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(stored), setup["secret"]) {
		t.Fatal("webhook secret was stored in plaintext")
	}
	request, _ = http.NewRequest(http.MethodGet, urlPath, nil)
	response, err = server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated settings status = %d, want 401", response.StatusCode)
	}
}

func TestGitHubWebhookCrashWindowsAreDurableAcrossSubprocessRestart(t *testing.T) {
	for _, mode := range []string{"before-commit", "after-commit", "before-ack", "after-ack"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			dbPath, keyPath := filepath.Join(dir, "state.db"), filepath.Join(dir, "opaque.key")
			if mode == "after-ack" {
				cmd, stdout := startWebhookCrashChild(t, mode, dbPath, keyPath, true)
				line, err := bufio.NewReader(stdout).ReadString('\n')
				if err != nil {
					t.Fatalf("webhook child did not start: %v", err)
				}
				parts := strings.Fields(line)
				if len(parts) != 3 {
					t.Fatalf("invalid child ready message %q", line)
				}
				secret, _ := hex.DecodeString(parts[2])
				client := &http.Client{Timeout: 5 * time.Second}
				status := sendWebhookWithProxyHeader(client, "http://"+parts[0]+"/webhooks/github/"+parts[1], "after-ack", "push", []byte(`{"ref":"refs/heads/main","after":"`+strings.Repeat("a", 40)+`"}`), secret)
				if status != http.StatusNoContent {
					_ = cmd.Process.Kill()
					_ = cmd.Wait()
					t.Fatalf("delivery status = %d, want 204 before killing process", status)
				}
				_ = cmd.Process.Kill()
				_ = cmd.Wait()
			} else {
				cmd, _ := startWebhookCrashChild(t, mode, dbPath, keyPath, false)
				err := cmd.Wait()
				var exitErr *exec.ExitError
				if !errors.As(err, &exitErr) || exitErr.ExitCode() != 71 {
					t.Fatalf("crash child error = %v, want exit 71 at %s", err, mode)
				}
			}
			app, err := New(Config{DatabasePath: dbPath, OPAQUEKeyPath: keyPath, GitTimeout: time.Second})
			if err != nil {
				t.Fatal(err)
			}
			defer app.Close()
			var receipts, jobs int
			if err = app.db.QueryRow(`SELECT count(*) FROM webhook_deliveries`).Scan(&receipts); err != nil {
				t.Fatal(err)
			}
			if err = app.db.QueryRow(`SELECT count(*) FROM deployment_jobs WHERE durable=1`).Scan(&jobs); err != nil {
				t.Fatal(err)
			}
			wantReceipt := mode != "before-commit"
			if (receipts > 0) != wantReceipt || (wantReceipt && jobs == 0) || (!wantReceipt && jobs != 0) {
				t.Fatalf("after %s crash: receipts=%d durable jobs=%d", mode, receipts, jobs)
			}
		})
	}
}

// Invoked as a child test process so os.Exit models an abrupt process loss,
// including the windows around the durable commit and HTTP acknowledgement.
func TestGitHubWebhookCrashChild(t *testing.T) {
	mode := os.Getenv("WEBHOOK_CRASH_MODE")
	if mode == "" {
		return
	}
	dbPath, keyPath := os.Getenv("WEBHOOK_CRASH_DB"), os.Getenv("WEBHOOK_CRASH_KEY")
	app, err := New(Config{DatabasePath: dbPath, OPAQUEKeyPath: keyPath, GitTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	projectID, secret := seedWebhookCrashProject(t, app)
	if mode == "serve" {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		fmt.Printf("%s %s %s\n", listener.Addr().String(), projectID, hex.EncodeToString(secret))
		server := &http.Server{Handler: app.Handler(), ReadTimeout: 10 * time.Second}
		_ = server.Serve(listener)
		return
	}
	point := mode
	if mode == "after-ack" {
		point = ""
	}
	app.webhookFault = func(at string) error {
		if at == point || (mode == "serve" && at == "before-ack") {
			os.Exit(71)
		}
		return nil
	}
	server := httptest.NewTLSServer(app.Handler())
	defer server.Close()
	if mode == "serve" {
		t.Fatal("invalid child mode")
	}
	if mode == "after-ack" {
		t.Fatal("after-ack child is started in serve mode")
	}
	body := []byte(`{"ref":"refs/heads/main","after":"` + strings.Repeat("a", 40) + `"}`)
	if os.Getenv("WEBHOOK_CRASH_PROVIDER") == "gitea" {
		_ = sendGiteaWebhook(server.Client(), server.URL+"/webhooks/gitea/"+projectID, "crash-window", "push", body, secret)
	} else {
		_ = sendGitHubWebhook(server.Client(), server.URL+"/webhooks/github/"+projectID, "crash-window", "push", body, secret)
	}
	t.Fatalf("expected abrupt exit at %s", point)
}

func startWebhookCrashChild(t *testing.T, mode, dbPath, keyPath string, serve bool) (*exec.Cmd, io.Reader) {
	t.Helper()
	childMode := mode
	if serve {
		childMode = "serve"
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestGitHubWebhookCrashChild$")
	cmd.Env = append(os.Environ(), "WEBHOOK_CRASH_MODE="+childMode, "WEBHOOK_CRASH_DB="+dbPath, "WEBHOOK_CRASH_KEY="+keyPath)
	if serve {
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		cmd.Stderr = os.Stderr
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		return cmd, stdout
	} else if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	return cmd, nil
}

func seedWebhookCrashProject(t *testing.T, app *App) (string, []byte) {
	t.Helper()
	projectID, err := randomID()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = app.db.Exec(`INSERT INTO projects(id,name,remote_url,created_at) VALUES(?,?,?,?)`, projectID, "crash fixture", "https://127.0.0.1:1/repo.git", time.Now().UnixNano()); err != nil {
		t.Fatal(err)
	}
	if _, err = app.db.Exec(`INSERT INTO deployments(id,project_id,name,commit_id,target_kind,target_name,generation,status,created_at,updated_at) VALUES(?,?,?,?,?,?,1,'live',?,?)`, strings.Repeat("b", 32), projectID, "production", strings.Repeat("a", 40), "branch", "main", time.Now().UnixNano(), time.Now().UnixNano()); err != nil {
		t.Fatal(err)
	}
	secret := []byte("subprocess test webhook secret")
	sealed, err := app.sealWebhookSecret(projectID, secret)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = app.db.Exec(`INSERT INTO project_webhooks(project_id,secret,created_at) VALUES(?,?,?)`, projectID, sealed, time.Now().UnixNano()); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("WEBHOOK_CRASH_PROVIDER") == "gitea" {
		sealed, err = app.sealProviderWebhookSecret("gitea", projectID, secret)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = app.db.Exec(`INSERT INTO project_gitea_webhooks(project_id,secret,created_at) VALUES(?,?,?)`, projectID, sealed, time.Now().UnixNano()); err != nil {
			t.Fatal(err)
		}
	}
	return projectID, secret
}

func sendWebhookWithProxyHeader(client *http.Client, url, id, event string, body, secret []byte) int {
	request, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(string(body)))
	request.Header.Set("X-Forwarded-Proto", "https")
	request.Header.Set("X-GitHub-Delivery", id)
	request.Header.Set("X-GitHub-Event", event)
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write(body)
	request.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	response, err := client.Do(request)
	if err != nil {
		return 0
	}
	defer response.Body.Close()
	return response.StatusCode
}

func webhookTestApp(t *testing.T, maxBody int64) (*App, string, []byte) {
	t.Helper()
	dir := t.TempDir()
	app, err := New(Config{DatabasePath: filepath.Join(dir, "state.db"), OPAQUEKeyPath: filepath.Join(dir, "opaque.key"), InsecureLocalHTTP: true, WebhookMaxBodyBytes: maxBody})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.Close() })
	projectID, err := randomID()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = app.db.Exec(`INSERT INTO projects(id,name,remote_url,created_at) VALUES(?,?,?,?)`, projectID, "webhook fixture", "https://example.invalid/repo.git", time.Now().UnixNano()); err != nil {
		t.Fatal(err)
	}
	secret := []byte("a dedicated signing secret for test only")
	sealed, err := app.sealWebhookSecret(projectID, secret)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = app.db.Exec(`INSERT INTO project_webhooks(project_id,secret,created_at) VALUES(?,?,?)`, projectID, sealed, time.Now().UnixNano()); err != nil {
		t.Fatal(err)
	}
	return app, projectID, secret
}

func sendGitHubWebhook(client *http.Client, url, id, event string, body, secret []byte) int {
	request, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(string(body)))
	request.Header.Set("X-GitHub-Delivery", id)
	request.Header.Set("X-GitHub-Event", event)
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write(body)
	request.Header.Set("X-Hub-Signature-256", fmt.Sprintf("sha256=%s", hex.EncodeToString(mac.Sum(nil))))
	response, err := client.Do(request)
	if err != nil {
		return 0
	}
	defer response.Body.Close()
	return response.StatusCode
}
