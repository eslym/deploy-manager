package lib

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLegacyGitHubWebhookUsesDisplayedSecretAfterUpgrade(t *testing.T) {
	f := newAutomationFixture(t, "github")
	id := f.createBranch(t, "legacy-webhook")
	// Reproduce the pre-upgrade on-disk format independently of the current
	// application's secret helpers: AES-GCM encrypted 32 random source bytes,
	// while setup displayed their 64-character hex encoding to the administrator.
	raw, err := hex.DecodeString(strings.Repeat("a1", 32))
	if err != nil {
		t.Fatal(err)
	}
	block, err := aes.NewCipher(f.app.repositoryMaster)
	if err != nil {
		t.Fatal(err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	nonce := make([]byte, gcm.NonceSize())
	sealed := gcm.Seal(nonce, nonce, raw, []byte("github-webhook:"+f.projectID))
	if _, err = f.app.db.Exec(`UPDATE project_webhooks SET secret=? WHERE project_id=?`, sealed, f.projectID); err != nil {
		t.Fatal(err)
	}
	restart := func() {
		t.Helper()
		cfg := f.app.cfg
		f.server.Close()
		if err := f.app.Close(); err != nil {
			t.Fatal(err)
		}
		f.app, err = New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		f.cookie = installAPITestSession(t, f.app)
		f.server = httptest.NewTLSServer(f.app.Handler())
	}
	restart()
	f.secret = []byte(strings.Repeat("a1", 32)) // literal value entered in GitHub
	latest := addSmartFixtureCommit(t, f.remoteDir, "legacy configured secret works")
	f.push(t, "legacy-after-upgrade", latest)
	jobs := f.waitJobs(t, id)
	if jobs[0].Status != "succeeded" || jobs[0].Commit != latest {
		t.Fatalf("legacy webhook did not publish after upgrade: %+v", jobs[0])
	}
	body := []byte(`{"ref":"refs/heads/feature/search","after":"` + latest + `"}`)
	url := f.server.URL + "/webhooks/github/" + f.projectID
	if status := sendGitHubWebhook(f.server.Client(), url, "legacy-raw-rejected", "push", body, raw); status != http.StatusUnauthorized {
		t.Fatalf("undisplayed raw source authenticated: %d", status)
	}
	response := projectPost(t, f.server.Client(), f.server.URL, f.cookie, "/api/projects/"+f.projectID+"/github-webhook", `{"rotate":true}`)
	var rotated struct {
		Code    string `json:"code"`
		Message string `json:"message"`
		Data    struct {
			URL    string `json:"url"`
			Secret string `json:"secret"`
		} `json:"data"`
	}
	decodeErr := json.NewDecoder(response.Body).Decode(&rotated)
	if response.StatusCode != http.StatusCreated || decodeErr != nil ||
		rotated.Code != "webhook.secret_rotated" || rotated.Message == "" ||
		!strings.HasPrefix(rotated.Data.URL, "/webhooks/github/") ||
		len(rotated.Data.Secret) != 64 {
		t.Fatalf("rotation failed: status=%d outcome=%+v decodeErr=%v", response.StatusCode, rotated, decodeErr)
	}
	response.Body.Close()
	restart()
	url = f.server.URL + "/webhooks/github/" + f.projectID
	if status := sendGitHubWebhook(f.server.Client(), url, "legacy-after-rotation", "push", body, f.secret); status != http.StatusUnauthorized {
		t.Fatalf("rotated legacy secret still authenticated: %d", status)
	}
	f.secret = []byte(rotated.Data.Secret)
	latest = addSmartFixtureCommit(t, f.remoteDir, "rotated literal secret works")
	f.push(t, "rotated-after-restart", latest)
	jobs = f.waitJobs(t, id)
	if jobs[0].Status != "succeeded" || jobs[0].Commit != latest {
		t.Fatalf("rotated webhook did not publish after restart: %+v", jobs[0])
	}
}
