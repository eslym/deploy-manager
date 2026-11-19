package lib

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

func TestPermanentRollbackCrashRestartRace(t *testing.T) {
	root := t.TempDir()
	marker := filepath.Join(root, "accepted.json")
	child := exec.Command(os.Args[0], "-test.run=^TestPermanentRollbackCrashRestartRaceChild$")
	child.Env = append(os.Environ(), "DM_AUTOMATION_FIXTURE_DIR="+root, "DM_ROLLBACK_CRASH_CHILD="+marker)
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if child.ProcessState == nil {
			_ = child.Process.Kill()
			_, _ = child.Process.Wait()
		}
	})
	markerBytes := waitForFile(t, marker)
	var state struct {
		DeploymentID string `json:"deploymentId"`
		JobID        string `json:"jobId"`
		LivePath     string `json:"livePath"`
		LiveContent  string `json:"liveContent"`
	}
	if err := json.Unmarshal(markerBytes, &state); err != nil {
		t.Fatal(err)
	}
	// Kill the application while the accepted rollback worker is still held at
	// its pre-publication boundary. A graceful close would release the job.
	if err := child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_, _ = child.Process.Wait()
	app, err := New(Config{DatabasePath: filepath.Join(root, "state.db"), OPAQUEKeyPath: filepath.Join(root, "opaque.key")})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	server := httptest.NewTLSServer(app.Handler())
	defer server.Close()
	cookie := installAPITestSession(t, app)
	assertAutomationPaused(t, server, cookie, state.DeploymentID, true)
	waitForDeploymentJob(t, server.Client(), server.URL, cookie, state.DeploymentID, state.JobID, "failed")
	content, err := os.ReadFile(filepath.Join(state.LivePath, "README.md"))
	if err != nil || string(content) != state.LiveContent {
		t.Fatalf("crash/recovery changed live publication: content=%q err=%v", content, err)
	}
}

func waitForFile(t *testing.T, path string) []byte {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(path); err == nil {
			return data
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("subprocess did not accept and pause the permanent rollback")
	return nil
}

func TestPermanentRollbackCrashRestartRaceChild(t *testing.T) {
	marker := os.Getenv("DM_ROLLBACK_CRASH_CHILD")
	if marker == "" {
		return
	}
	f := newAutomationFixture(t)
	id := f.createBranch(t, "permanent-rollback-process-crash")
	livePath := f.app.deploymentPath(id)
	live, err := os.ReadFile(filepath.Join(livePath, "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	previous := setPublicationFaultPoint(func(point string) error {
		if point == "before-job-lock" {
			close(started)
			select {}
		}
		return nil
	})
	defer setPublicationFaultPoint(previous)
	response := projectPost(t, f.server.Client(), f.server.URL, f.cookie, "/api/deployments/"+id+"/rollback", `{"commit":"`+f.initialCommit+`","permanent":true}`)
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("rollback request status=%d body=%s", response.StatusCode, responseText(response))
	}
	var accepted struct {
		Code string `json:"code"`
		Data struct {
			JobID string `json:"jobId"`
		} `json:"data"`
	}
	if err := json.NewDecoder(response.Body).Decode(&accepted); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("rollback worker did not block before publication")
	}
	assertAutomationPaused(t, f.server, f.cookie, id, true)
	data, err := json.Marshal(map[string]string{"deploymentId": id, "jobId": accepted.Data.JobID, "livePath": livePath, "liveContent": string(live)})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marker, data, 0600); err != nil {
		t.Fatal(err)
	}
	select {}
}

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
	var queued struct {
		Code string `json:"code"`
		Data struct {
			JobID string `json:"jobId"`
		} `json:"data"`
	}
	if err = json.NewDecoder(response.Body).Decode(&queued); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	waitForDeploymentJob(t, f.server.Client(), f.server.URL, f.cookie, id, queued.Data.JobID, "failed")
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
	var queued struct {
		Code string `json:"code"`
		Data struct {
			JobID string `json:"jobId"`
		} `json:"data"`
	}
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
	waitForDeploymentJob(t, f.server.Client(), f.server.URL, f.cookie, id, queued.Data.JobID, "failed")
	assertAutomationPaused(t, f.server, f.cookie, id, true)
	request, _ := http.NewRequest(http.MethodGet, f.server.URL+"/api/deployments", nil)
	request.AddCookie(&http.Cookie{Name: sessionCookie, Value: f.cookie})
	response, err = f.server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	var deployments struct {
		Code string `json:"code"`
		Data []struct {
			ID    string `json:"id"`
			Error string `json:"error"`
		} `json:"data"`
	}
	if err = json.NewDecoder(response.Body).Decode(&deployments); err != nil {
		response.Body.Close()
		t.Fatal(err)
	}
	response.Body.Close()
	errorVisible := false
	for _, deployment := range deployments.Data {
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
				f.fetchFailureStatus.Store(int32(http.StatusServiceUnavailable))
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
			var deployments struct {
				Code string `json:"code"`
				Data []struct {
					ID         string `json:"id"`
					TargetKind string `json:"targetKind"`
					TargetName string `json:"targetName"`
					Error      string `json:"error"`
				} `json:"data"`
			}
			if err = json.NewDecoder(response.Body).Decode(&deployments); err != nil {
				response.Body.Close()
				t.Fatal(err)
			}
			response.Body.Close()
			found := false
			for _, deployment := range deployments.Data {
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
	var deployments struct {
		Code string `json:"code"`
		Data []struct {
			ID, TargetKind, TargetName string
		} `json:"data"`
	}
	if err = json.NewDecoder(response.Body).Decode(&deployments); err != nil {
		response.Body.Close()
		t.Fatal(err)
	}
	response.Body.Close()
	for _, deployment := range deployments.Data {
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
