package lib

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestPermanentRollbackPausesAtRequestEvenWhenPublicationFails(t *testing.T) {
	f := newAutomationFixture(t)
	id := f.createBranch(t, "permanent-rollback")
	other := f.createBranch(t, "permanent-rollback-independent")
	before, err := identity(f.app.deploymentPath(id))
	if err != nil {
		t.Fatal(err)
	}
	missing := strings.Repeat("f", 40)
	response := projectPost(t, f.server.Client(), f.server.URL, f.cookie, "/api/deployments/"+id+"/rollback", `{"commit":"`+missing+`","permanent":true}`)
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("permanent rollback request: %d %s", response.StatusCode, responseText(response))
	}
	var queued struct{ JobID string }
	if err = json.NewDecoder(response.Body).Decode(&queued); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	waitForDeploymentJob(t, f.server.Client(), f.server.URL, f.cookie, id, queued.JobID, "failed")
	assertAutomationPaused(t, f.server, f.cookie, id, true)
	if after, err := identity(f.app.deploymentPath(id)); err != nil || after != before {
		t.Fatalf("failed permanent rollback changed live publication: %v", err)
	}
	cfg := f.app.cfg
	f.server.Close()
	if err = f.app.Close(); err != nil {
		t.Fatal(err)
	}
	f.app, err = New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	f.cookie = installAPITestSession(t, f.app)
	f.server = httptest.NewTLSServer(f.app.Handler())
	defer f.server.Close()
	defer f.app.Close()
	assertAutomationPaused(t, f.server, f.cookie, id, true)
	latest := addSmartFixtureCommit(t, f.remoteDir, "after permanent rollback failure")
	f.push(t, "after-permanent-rollback-restart", latest)
	pausedJobs := f.waitJobs(t, id)
	if pausedJobs[0].Status != "noop" {
		t.Fatalf("permanently paused deployment published webhook update: %+v", pausedJobs[0])
	}
	independentJobs := f.waitJobs(t, other)
	if independentJobs[0].Status != "succeeded" || independentJobs[0].Commit != latest {
		t.Fatalf("permanent pause blocked independent deployment: %+v", independentJobs[0])
	}
	if after, err := identity(f.app.deploymentPath(id)); err != nil || after != before {
		t.Fatalf("restart or paused webhook changed failed rollback publication: %v", err)
	}
}

func TestPermanentRollbackPauseIsVisibleBeforePublicationWork(t *testing.T) {
	f := newAutomationFixture(t)
	id := f.createBranch(t, "permanent-rollback-request")
	before, err := identity(f.app.deploymentPath(id))
	if err != nil {
		t.Fatal(err)
	}
	workerStarted, releaseWorker := make(chan struct{}), make(chan struct{})
	var release sync.Once
	unblock := func() { release.Do(func() { close(releaseWorker) }) }
	defer unblock()
	previous := setPublicationFaultPoint(func(point string) error {
		if point == "before-job-lock" {
			close(workerStarted)
			<-releaseWorker
		}
		if point == "after-stage-fsync" {
			return errors.New("injected publication preparation failure")
		}
		return nil
	})
	defer setPublicationFaultPoint(previous)
	response := projectPost(t, f.server.Client(), f.server.URL, f.cookie, "/api/deployments/"+id+"/rollback", `{"commit":"`+f.initialCommit+`","permanent":true}`)
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("permanent rollback request: %d %s", response.StatusCode, responseText(response))
	}
	var queued struct{ JobID string }
	if err = json.NewDecoder(response.Body).Decode(&queued); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	select {
	case <-workerStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("rollback worker did not reach the controlled boundary")
	}
	assertAutomationPaused(t, f.server, f.cookie, id, true)
	unblock()
	waitForDeploymentJob(t, f.server.Client(), f.server.URL, f.cookie, id, queued.JobID, "failed")
	assertAutomationPaused(t, f.server, f.cookie, id, true)
	request, _ := http.NewRequest(http.MethodGet, f.server.URL+"/api/deployments", nil)
	request.AddCookie(&http.Cookie{Name: sessionCookie, Value: f.cookie})
	response, err = f.server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	var deployments []struct {
		ID    string `json:"id"`
		Error string `json:"error"`
	}
	if err = json.NewDecoder(response.Body).Decode(&deployments); err != nil {
		response.Body.Close()
		t.Fatal(err)
	}
	response.Body.Close()
	errorVisible := false
	for _, deployment := range deployments {
		if deployment.ID == id {
			if deployment.Error == "" {
				t.Fatal("failed permanent rollback did not expose an error in deployment state")
			}
			errorVisible = true
			break
		}
	}
	if !errorVisible {
		t.Fatal("failed deployment is missing from dashboard state")
	}
	if after, err := identity(f.app.deploymentPath(id)); err != nil || after != before {
		t.Fatalf("failed permanent rollback changed live publication: %v", err)
	}
}

func TestTemporaryRollbackSurvivesRestartWithoutUnsolicitedCatchUp(t *testing.T) {
	for _, provider := range []string{"github", "gitea"} {
		t.Run(provider, func(t *testing.T) {
			f := newAutomationFixture(t, provider)
			id := f.createBranch(t, "rollback-restart")
			latest := addSmartFixtureCommit(t, f.remoteDir, "newest branch content")
			job := f.action(t, id, "deploy", `{}`)
			waitForDeploymentJob(t, f.server.Client(), f.server.URL, f.cookie, id, job, "succeeded")
			job = f.action(t, id, "rollback", `{"commit":"`+f.initialCommit+`"}`)
			waitForDeploymentJob(t, f.server.Client(), f.server.URL, f.cookie, id, job, "succeeded")
			beforeJobs := f.waitJobs(t, id)
			beforeFetches := f.gitFetches.Load()
			path := filepath.Join(f.app.deploymentPath(id), "README.md")
			cfg := f.app.cfg
			cfg.DeployConcurrency = 1
			f.server.Close()
			if err := f.app.Close(); err != nil {
				t.Fatal(err)
			}
			var err error
			f.app, err = New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			f.cookie = installAPITestSession(t, f.app)
			f.server = httptest.NewTLSServer(f.app.Handler())
			assertAutomationPaused(t, f.server, f.cookie, id, false)
			// With one worker, completing a new deployment fences startup's
			// restored queue: any unsolicited catch-up queued at restart must
			// already have run. This replaces the scheduler-dependent quiet sleep.
			f.createBranch(t, "restart-queue-fence")
			if jobs := f.waitJobs(t, id); len(jobs) != len(beforeJobs) || f.gitFetches.Load() != beforeFetches+1 {
				t.Fatalf("restart scheduled unsolicited tip work: jobs %d -> %d, fetches %d -> %d", len(beforeJobs), len(jobs), beforeFetches, f.gitFetches.Load())
			}
			if content, err := os.ReadFile(path); err != nil || string(content) != "searchable feature\n" {
				t.Fatalf("restart lost rollback: %q %v", content, err)
			}
			f.push(t, "later-matching-push", latest)
			jobs := f.waitJobs(t, id)
			if jobs[0].Status != "succeeded" || jobs[0].Commit != latest {
				t.Fatalf("later webhook did not replace rollback: %+v", jobs[0])
			}
			if content, err := os.ReadFile(path); err != nil || string(content) != "newest branch content\n" {
				t.Fatalf("later webhook content %q: %v", content, err)
			}
		})
	}
}
