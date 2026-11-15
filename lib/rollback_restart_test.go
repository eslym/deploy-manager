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

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
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

func TestPermanentRollbackFailuresKeepPauseTargetAndLiveFiles(t *testing.T) {
	for _, phase := range []string{"fetch", "missing-branch", "missing-content", "ancestry-invalidation", "publication"} {
		t.Run(phase, func(t *testing.T) {
			f := newAutomationFixture(t)
			id := f.createBranch(t, "permanent-rollback-"+phase)
			path := f.app.deploymentPath(id)
			before, err := identity(path)
			if err != nil {
				t.Fatal(err)
			}
			selected := f.initialCommit
			restoreFault := func() {}
			switch phase {
			case "fetch":
				f.failGitFetch.Store(true)
			case "missing-branch":
				remote, openErr := git.PlainOpen(f.remoteDir)
				if openErr != nil {
					t.Fatal(openErr)
				}
				if removeErr := remote.Storer.RemoveReference(plumbing.NewBranchReferenceName("feature/search")); removeErr != nil {
					t.Fatal(removeErr)
				}
				cache, openErr := git.PlainOpen(f.app.projects.repoPath(f.projectID))
				if openErr != nil {
					t.Fatal(openErr)
				}
				if removeErr := cache.Storer.RemoveReference(plumbing.NewRemoteReferenceName("origin", "feature/search")); removeErr != nil {
					t.Fatal(removeErr)
				}
			case "missing-content":
				repo, openErr := git.PlainOpen(f.remoteDir)
				if openErr != nil {
					t.Fatal(openErr)
				}
				branch := plumbing.NewBranchReferenceName("feature/search")
				tip, refErr := repo.Reference(branch, true)
				if refErr != nil {
					t.Fatal(refErr)
				}
				existing, commitErr := repo.CommitObject(tip.Hash())
				if commitErr != nil {
					t.Fatal(commitErr)
				}
				sig := object.Signature{Name: "Rollback fixture", Email: "rollback@example.test", When: time.Now().UTC()}
				broken := object.Commit{Author: sig, Committer: sig, TreeHash: plumbing.NewHash(strings.Repeat("f", 40)), ParentHashes: []plumbing.Hash{tip.Hash()}, Message: "missing rollback tree"}
				encoded := repo.Storer.NewEncodedObject()
				if err = broken.Encode(encoded); err != nil {
					t.Fatal(err)
				}
				selectedHash, setErr := repo.Storer.SetEncodedObject(encoded)
				if setErr != nil {
					t.Fatal(setErr)
				}
				selected = selectedHash.String()
				descendant := object.Commit{Author: sig, Committer: sig, TreeHash: existing.TreeHash, ParentHashes: []plumbing.Hash{selectedHash}, Message: "branch tip after missing rollback tree"}
				encoded = repo.Storer.NewEncodedObject()
				if err = descendant.Encode(encoded); err != nil {
					t.Fatal(err)
				}
				descendantHash, setErr := repo.Storer.SetEncodedObject(encoded)
				if setErr != nil {
					t.Fatal(setErr)
				}
				if setErr = repo.Storer.SetReference(plumbing.NewHashReference(branch, descendantHash)); setErr != nil {
					t.Fatal(setErr)
				}
			case "ancestry-invalidation":
				repo, openErr := git.PlainOpen(f.remoteDir)
				if openErr != nil {
					t.Fatal(openErr)
				}
				main, refErr := repo.Reference(plumbing.NewBranchReferenceName("main"), true)
				if refErr != nil {
					t.Fatal(refErr)
				}
				if setErr := repo.Storer.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName("feature/search"), main.Hash())); setErr != nil {
					t.Fatal(setErr)
				}
			case "publication":
				previous := setPublicationFaultPoint(func(point string) error {
					if point == "after-stage-fsync" {
						return errors.New("injected permanent rollback publication failure")
					}
					return nil
				})
				restoreFault = func() { setPublicationFaultPoint(previous) }
			}
			defer restoreFault()
			job := f.action(t, id, "rollback", `{"commit":"`+selected+`","permanent":true}`)
			waitForDeploymentJob(t, f.server.Client(), f.server.URL, f.cookie, id, job, "failed")
			restoreFault()
			assertAutomationPaused(t, f.server, f.cookie, id, true)
			var target struct {
				TargetKind string `json:"targetKind"`
				TargetName string `json:"targetName"`
				Error      string `json:"error"`
			}
			request, _ := http.NewRequest(http.MethodGet, f.server.URL+"/api/deployments", nil)
			request.AddCookie(&http.Cookie{Name: sessionCookie, Value: f.cookie})
			response, err := f.server.Client().Do(request)
			if err != nil {
				t.Fatal(err)
			}
			var deployments []struct {
				ID         string `json:"id"`
				TargetKind string `json:"targetKind"`
				TargetName string `json:"targetName"`
				Error      string `json:"error"`
			}
			if err = json.NewDecoder(response.Body).Decode(&deployments); err != nil {
				response.Body.Close()
				t.Fatal(err)
			}
			response.Body.Close()
			found := false
			for _, deployment := range deployments {
				if deployment.ID == id {
					target.TargetKind, target.TargetName, target.Error = deployment.TargetKind, deployment.TargetName, deployment.Error
					found = true
				}
			}
			if !found || target.TargetKind != "branch" || target.TargetName != "feature/search" || target.Error == "" {
				t.Fatalf("permanent rollback failure did not preserve branch target and expose error: %+v", target)
			}
			if after, err := identity(path); err != nil || after != before {
				t.Fatalf("%s failure changed live publication identity: %v", phase, err)
			}
			content, err := os.ReadFile(filepath.Join(path, "README.md"))
			if err != nil || string(content) != "searchable feature\n" {
				t.Fatalf("%s failure changed live files: %q %v", phase, content, err)
			}
		})
	}
}

func TestPermanentRollbackIsSupersededByBranchTargetChange(t *testing.T) {
	f := newAutomationFixture(t)
	id := f.createBranch(t, "permanent-rollback-target-change")
	started, release := make(chan struct{}), make(chan struct{})
	var once, unblock sync.Once
	defer unblock.Do(func() { close(release) })
	previous := setPublicationFaultPoint(func(point string) error {
		if point == "before-job-lock" {
			once.Do(func() { close(started); <-release })
		}
		return nil
	})
	defer setPublicationFaultPoint(previous)
	rollback := f.action(t, id, "rollback", `{"commit":"`+f.initialCommit+`","permanent":true}`)
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("permanent rollback did not reach the controlled boundary")
	}
	targetChange := f.action(t, id, "target", `{"targetKind":"branch","targetName":"main"}`)
	unblock.Do(func() { close(release) })
	waitForDeploymentJob(t, f.server.Client(), f.server.URL, f.cookie, id, rollback, "superseded")
	waitForDeploymentJob(t, f.server.Client(), f.server.URL, f.cookie, id, targetChange, "succeeded")
	assertAutomationPaused(t, f.server, f.cookie, id, true)
	var found bool
	request, _ := http.NewRequest(http.MethodGet, f.server.URL+"/api/deployments", nil)
	request.AddCookie(&http.Cookie{Name: sessionCookie, Value: f.cookie})
	response, err := f.server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	var deployments []struct {
		ID, TargetKind, TargetName string
	}
	if err = json.NewDecoder(response.Body).Decode(&deployments); err != nil {
		response.Body.Close()
		t.Fatal(err)
	}
	response.Body.Close()
	for _, deployment := range deployments {
		if deployment.ID == id {
			found = deployment.TargetKind == "branch" && deployment.TargetName == "main"
		}
	}
	if !found {
		t.Fatal("permanent rollback or target change altered the requested branch target")
	}
	if content, err := os.ReadFile(filepath.Join(f.app.deploymentPath(id), "README.md")); err != nil || string(content) != "main content\n" {
		t.Fatalf("branch target change did not publish its selected target: %q %v", content, err)
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
