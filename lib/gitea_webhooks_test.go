package lib

import (
	"bufio"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGiteaWebhookSetupIsProtectedAndProviderIsolated(t *testing.T) {
	app, projectID, githubSecret := webhookTestApp(t, 1024)
	server := httptest.NewTLSServer(app.Handler())
	defer server.Close()
	token := []byte(strings.Repeat("g", 32))
	digest := sha256.Sum256(token)
	app.sessions[digest] = sessionState{expires: time.Now().Add(time.Hour), generation: 0, authenticatedAt: time.Now()}
	cookie := base64.RawURLEncoding.EncodeToString(token)
	path := "/api/projects/" + projectID + "/gitea-webhook"
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		req, _ := http.NewRequest(method, server.URL+path, strings.NewReader(`{"rotate":true}`))
		response, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusUnauthorized {
			t.Fatalf("unauthenticated %s = %d", method, response.StatusCode)
		}
	}
	req, _ := http.NewRequest(http.MethodPost, server.URL+path, strings.NewReader(`{"rotate":true}`))
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: cookie})
	req.Header.Set("Origin", "https://attacker.invalid")
	response, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin rotation = %d", response.StatusCode)
	}
	secret := setupProviderWebhook(t, server, cookie, projectID, "gitea")
	req, _ = http.NewRequest(http.MethodGet, server.URL+path, nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: cookie})
	response, err = server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var settings struct {
		Code string         `json:"code"`
		Data map[string]any `json:"data"`
	}
	_ = json.NewDecoder(response.Body).Decode(&settings)
	response.Body.Close()
	if response.StatusCode != 200 || settings.Code != "webhook.settings_loaded" || settings.Data["configured"] != true || settings.Data["secret"] != nil {
		t.Fatalf("settings = %d %#v", response.StatusCode, settings)
	}
	body := []byte(`{"ref":"refs/heads/main","after":"` + strings.Repeat("a", 40) + `"}`)
	url := server.URL + "/webhooks/gitea/" + projectID
	if status := sendGiteaWebhook(server.Client(), url, "delivery", "push", body, secret); status != 200 {
		t.Fatalf("valid Gitea push = %d", status)
	}
	if status := sendGiteaWebhook(server.Client(), url, "delivery", "push", body, githubSecret); status != 401 {
		t.Fatalf("GitHub secret accepted for Gitea: %d", status)
	}
	if status := sendGitHubWebhook(server.Client(), server.URL+"/webhooks/github/"+projectID, "delivery", "push", body, githubSecret); status != 200 {
		t.Fatalf("GitHub regression: %d", status)
	}
	rotated := setupProviderWebhook(t, server, cookie, projectID, "gitea")
	if string(rotated) == string(secret) {
		t.Fatal("rotation reused secret")
	}
	if status := sendGiteaWebhook(server.Client(), url, "delivery", "push", body, secret); status != 401 {
		t.Fatalf("old secret still accepted: %d", status)
	}
	if status := sendGiteaWebhook(server.Client(), url, "delivery", "push", body, rotated); status != 200 {
		t.Fatalf("rotated secret = %d", status)
	}
}

func TestProviderWebhookAcceptanceUsesStructuredOutcome(t *testing.T) {
	app, projectID, githubSecret := webhookTestApp(t, 2048)
	defer app.Close()
	token := []byte(strings.Repeat("w", 32))
	app.sessions[sha256.Sum256(token)] = sessionState{expires: time.Now().Add(time.Hour)}
	cookie := base64.RawURLEncoding.EncodeToString(token)
	server := httptest.NewTLSServer(app.Handler())
	defer server.Close()
	giteaSecret := setupProviderWebhook(t, server, cookie, projectID, "gitea")
	body := []byte(`{"ref":"refs/heads/unmatched","after":"` + strings.Repeat("a", 40) + `"}`)

	for _, tc := range []struct {
		provider string
		secret   []byte
	}{
		{provider: "github", secret: githubSecret},
		{provider: "gitea", secret: giteaSecret},
	} {
		t.Run(tc.provider, func(t *testing.T) {
			request, err := http.NewRequest(http.MethodPost, server.URL+"/webhooks/"+tc.provider+"/"+projectID, strings.NewReader(string(body)))
			if err != nil {
				t.Fatal(err)
			}
			digest := hmac.New(sha256.New, tc.secret)
			_, _ = digest.Write(body)
			if tc.provider == "github" {
				request.Header.Set("X-GitHub-Delivery", "structured-"+tc.provider)
				request.Header.Set("X-GitHub-Event", "push")
				request.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(digest.Sum(nil)))
			} else {
				request.Header.Set("X-Gitea-Delivery", "structured-"+tc.provider)
				request.Header.Set("X-Gitea-Event", "push")
				request.Header.Set("X-Gitea-Signature", hex.EncodeToString(digest.Sum(nil)))
			}
			response, err := server.Client().Do(request)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			var outcome struct {
				Code    string          `json:"code"`
				Message string          `json:"message"`
				Data    json.RawMessage `json:"data"`
			}
			if err := json.NewDecoder(response.Body).Decode(&outcome); err != nil {
				t.Fatal(err)
			}
			if response.StatusCode != http.StatusOK || outcome.Code != "webhook.accepted" || outcome.Message == "" || string(outcome.Data) != "null" {
				t.Fatalf("webhook outcome = %d %+v", response.StatusCode, outcome)
			}
		})
	}
}

func setupProviderWebhook(t *testing.T, server *httptest.Server, cookie, projectID, provider string) []byte {
	t.Helper()
	response := projectPost(t, server.Client(), server.URL, cookie, "/api/projects/"+projectID+"/"+provider+"-webhook", `{"rotate":true}`)
	var setup struct {
		Code string `json:"code"`
		Data struct {
			URL    string `json:"url"`
			Secret string `json:"secret"`
		} `json:"data"`
	}
	err := json.NewDecoder(response.Body).Decode(&setup)
	response.Body.Close()
	if response.StatusCode != 201 || err != nil || setup.Code != "webhook.secret_rotated" || setup.Data.URL != "/webhooks/"+provider+"/"+projectID || len(setup.Data.Secret) != 64 {
		t.Fatalf("%s setup = %d %+v (%v)", provider, response.StatusCode, setup, err)
	}
	// Providers use the literal secret copied from the setup page as the HMAC
	// key, not the bytes represented by its printable encoding.
	return []byte(setup.Data.Secret)
}

func sendGiteaWebhook(client *http.Client, url, id, event string, body, secret []byte) int {
	request, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(string(body)))
	request.Header.Set("X-Gitea-Delivery", id)
	request.Header.Set("X-Gitea-Event", event)
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write(body)
	request.Header.Set("X-Gitea-Signature", hex.EncodeToString(mac.Sum(nil)))
	response, err := client.Do(request)
	if err != nil {
		return 0
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusOK && !validWebhookAcceptedResponse(response) {
		return 0
	}
	return response.StatusCode
}

func TestGiteaWebhookRejectsInvalidRequestsAndIgnoresNonBranchEvents(t *testing.T) {
	app, id, _ := webhookTestApp(t, 1024)
	server := httptest.NewTLSServer(app.Handler())
	defer server.Close()
	token := []byte(strings.Repeat("h", 32))
	app.sessions[sha256.Sum256(token)] = sessionState{expires: time.Now().Add(time.Hour)}
	secret := setupProviderWebhook(t, server, base64.RawURLEncoding.EncodeToString(token), id, "gitea")
	url := server.URL + "/webhooks/gitea/" + id
	for _, tc := range []struct {
		name, event, body string
		want              int
	}{
		{"oversized", "push", strings.Repeat("x", 1025), 413},
		{"invalid-json", "push", `{`, 400},
		{"invalid-ref", "push", `{"ref":"refs/heads/a..b","after":"` + strings.Repeat("a", 40) + `"}`, 400},
		{"invalid-sha", "push", `{"ref":"refs/heads/main","after":"bad"}`, 400},
		{"tag-push", "push", `{"ref":"refs/tags/v1","after":"` + strings.Repeat("a", 40) + `"}`, 200},
		{"delete-push", "push", `{"ref":"refs/heads/main","after":"` + strings.Repeat("0", 40) + `"}`, 200},
		{"delete-event", "delete", `{"ref":"main","ref_type":"branch"}`, 200},
		{"unsupported", "issues", `{"action":"opened"}`, 200},
		{"missing-id", "push", `{}`, 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			delivery := tc.name
			if tc.name == "missing-id" {
				delivery = ""
			}
			if status := sendGiteaWebhook(server.Client(), url, delivery, tc.event, []byte(tc.body), secret); status != tc.want {
				t.Fatalf("status = %d, want %d", status, tc.want)
			}
		})
	}
	// GitHub-compatible headers must never substitute for Gitea headers.
	if status := sendGitHubWebhook(server.Client(), url, "github-only", "push", []byte(`{}`), secret); status != 400 {
		t.Fatalf("GitHub headers accepted at Gitea endpoint: %d", status)
	}
	if status := sendGiteaWebhook(server.Client(), url, "wrong-key", "push", []byte(`{}`), []byte("wrong")); status != 401 {
		t.Fatalf("wrong signing key = %d", status)
	}
}

func TestGiteaWebhookAcceptedWorkSurvivesProcessLoss(t *testing.T) {
	t.Setenv("WEBHOOK_CRASH_PROVIDER", "gitea")
	for _, mode := range []string{"before-commit", "after-commit", "before-ack", "after-ack"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			db, key := filepath.Join(dir, "state.db"), filepath.Join(dir, "opaque.key")
			if mode == "after-ack" {
				child, stdout := startWebhookCrashChild(t, mode, db, key, true)
				defer child.Process.Kill()
				line, err := bufio.NewReader(stdout).ReadString('\n')
				parts := strings.Fields(line)
				if err != nil || len(parts) != 3 {
					t.Fatalf("child ready = %q (%v)", line, err)
				}
				secret, _ := hex.DecodeString(parts[2])
				body := `{"ref":"refs/heads/main","after":"` + strings.Repeat("a", 40) + `"}`
				req, _ := http.NewRequest(http.MethodPost, "http://"+parts[0]+"/webhooks/gitea/"+parts[1], strings.NewReader(body))
				req.Header.Set("X-Forwarded-Proto", "https")
				req.Header.Set("X-Gitea-Delivery", "crash-window")
				req.Header.Set("X-Gitea-Event", "push")
				mac := hmac.New(sha256.New, secret)
				_, _ = mac.Write([]byte(body))
				req.Header.Set("X-Gitea-Signature", hex.EncodeToString(mac.Sum(nil)))
				res, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
				if err != nil {
					t.Fatal(err)
				}
				res.Body.Close()
				if res.StatusCode != 200 {
					t.Fatalf("before process loss acknowledgement = %d", res.StatusCode)
				}
				_ = child.Process.Kill()
				_ = child.Wait()
			} else {
				child, _ := startWebhookCrashChild(t, mode, db, key, false)
				var exit *exec.ExitError
				if err := child.Wait(); !errors.As(err, &exit) || exit.ExitCode() != 71 {
					t.Fatalf("child did not lose process at %s: %v", mode, err)
				}
			}
			app, err := New(Config{DatabasePath: db, OPAQUEKeyPath: key, GitTimeout: time.Second, AuthRequestsPerMinute: 1000})
			if err != nil {
				t.Fatal(err)
			}
			defer app.Close()
			server := httptest.NewTLSServer(app.Handler())
			defer server.Close()
			cookie := installAPITestSession(t, app)
			projects := listProjectsFromAPI(t, server.Client(), server.URL, cookie)
			if len(projects) != 1 {
				t.Fatalf("projects after restart: %+v", projects)
			}
			deploymentID := strings.Repeat("b", 32)
			waitForPublicJobsSettled(t, server, cookie, deploymentID)
			before := publicDeploymentJobs(t, server, cookie, deploymentID)
			if (len(before) > 0) != (mode != "before-commit") {
				t.Fatalf("jobs after %s crash = %+v", mode, before)
			}
			if mode == "before-commit" {
				return
			}
			body := []byte(`{"ref":"refs/heads/main","after":"` + strings.Repeat("a", 40) + `"}`)
			if status := sendGiteaWebhook(server.Client(), server.URL+"/webhooks/gitea/"+projects[0].ID, "crash-window", "push", body, []byte("subprocess test webhook secret")); status != 200 {
				t.Fatalf("retry after process loss = %d", status)
			}
			if after := publicDeploymentJobs(t, server, cookie, deploymentID); len(after) != len(before) {
				t.Fatalf("retry created extra jobs: before=%d after=%d", len(before), len(after))
			}
		})
	}
}
