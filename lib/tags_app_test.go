package lib

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

type tagDeploymentFixture struct {
	ID, JobID, FrozenCommit string
	Path                    string
	Identity                dirIdentity
}

func createAndTestTagDeployment(t *testing.T, client *http.Client, origin, cookie string, app *App, projectID string, refs []projectRef) tagDeploymentFixture {
	t.Helper()
	tagRef := findProjectRef(refs, "tag", "v1.0")
	response := projectPost(t, client, origin, cookie, "/api/deployments", `{"projectId":"`+projectID+`","name":"tag-production","targetKind":"tag","targetName":"v1.0"}`)
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("tag deployment creation status %d: %s", response.StatusCode, responseText(response))
	}
	var created struct {
		Code string `json:"code"`
		Data struct {
			ID    string `json:"id"`
			JobID string `json:"jobId"`
		} `json:"data"`
	}
	if err := json.NewDecoder(response.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if created.Code != "deployment.created" {
		t.Fatalf("tag deployment outcome=%#v", created)
	}
	waitForDeploymentJob(t, client, origin, cookie, created.Data.ID, created.Data.JobID, "succeeded")
	var frozen string
	if err := app.db.QueryRow(`SELECT commit_id FROM deployments WHERE id=?`, created.Data.ID).Scan(&frozen); err != nil || frozen != tagRef.Commit {
		t.Fatalf("created tag target resolution=%q, cached tag=%q, err=%v", frozen, tagRef.Commit, err)
	}
	path := filepath.Join(app.cfg.DeploymentRoot, created.Data.ID)
	identityBefore, err := identity(path)
	if err != nil {
		t.Fatal(err)
	}
	ordinary := projectPost(t, client, origin, cookie, "/api/deployments/"+created.Data.ID+"/deploy", `{}`)
	if ordinary.StatusCode != http.StatusAccepted {
		t.Fatalf("ordinary tag Deploy status %d: %s", ordinary.StatusCode, responseText(ordinary))
	}
	var ordinaryJob struct {
		Code string `json:"code"`
		Data struct {
			JobID string `json:"jobId"`
		} `json:"data"`
	}
	if err := json.NewDecoder(ordinary.Body).Decode(&ordinaryJob); err != nil {
		t.Fatal(err)
	}
	ordinary.Body.Close()
	if ordinaryJob.Code != "deployment.queued" {
		t.Fatalf("ordinary Deploy outcome=%#v", ordinaryJob)
	}
	waitForDeploymentJob(t, client, origin, cookie, created.Data.ID, ordinaryJob.Data.JobID, "noop")
	if after, identityErr := identity(path); identityErr != nil || after != identityBefore {
		t.Fatalf("ordinary tag Deploy recreated unchanged live content: before=%+v after=%+v err=%v", identityBefore, after, identityErr)
	}
	hardJob := hardRedeployAndWait(t, client, origin, cookie, created.Data.ID, path, nil)
	return tagDeploymentFixture{ID: created.Data.ID, JobID: hardJob, FrozenCommit: frozen, Path: path, Identity: identityBefore}
}

func hardRedeployAndWait(t *testing.T, client *http.Client, origin, cookie, deploymentID, path string, fetchCount *atomic.Int32) string {
	t.Helper()
	before, err := identity(path)
	if err != nil {
		t.Fatal(err)
	}
	baseline := int32(0)
	if fetchCount != nil {
		baseline = fetchCount.Load()
	}
	response := projectPost(t, client, origin, cookie, "/api/deployments/"+deploymentID+"/hard-redeploy", `{}`)
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("Hard re-deploy status %d: %s", response.StatusCode, responseText(response))
	}
	var queued struct {
		Code string `json:"code"`
		Data struct {
			JobID string `json:"jobId"`
		} `json:"data"`
	}
	if err := json.NewDecoder(response.Body).Decode(&queued); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if queued.Code != "deployment.hard_redeploy_queued" {
		t.Fatalf("Hard re-deploy outcome=%#v", queued)
	}
	waitForDeploymentJob(t, client, origin, cookie, deploymentID, queued.Data.JobID, "succeeded")
	if fetchCount != nil && fetchCount.Load() <= baseline {
		t.Fatal("Hard re-deploy did not fetch the repository")
	}
	if after, identityErr := identity(path); identityErr != nil || after == before {
		t.Fatalf("Hard re-deploy did not recreate the publication: before=%+v after=%+v err=%v", before, after, identityErr)
	}
	return queued.Data.JobID
}

func moveAnnotatedTagAndTestHardRefresh(t *testing.T, client *http.Client, origin, cookie string, app *App, fixtureRepo *git.Repository, failRequests *atomic.Bool, deployment tagDeploymentFixture, newCommit string, contents []byte) string {
	t.Helper()
	if err := fixtureRepo.Storer.RemoveReference(plumbing.NewTagReferenceName("v1.0")); err != nil {
		t.Fatal(err)
	}
	if _, err := fixtureRepo.CreateTag("v1.0", plumbing.NewHash(newCommit), &git.CreateTagOptions{Tagger: &object.Signature{Name: "fixture", Email: "fixture@example.test", When: time.Now().UTC()}, Message: "moved annotated release"}); err != nil {
		t.Fatal(err)
	}
	ordinary := projectPost(t, client, origin, cookie, "/api/deployments/"+deployment.ID+"/deploy", `{}`)
	if ordinary.StatusCode != http.StatusAccepted {
		t.Fatalf("ordinary Deploy for moved upstream tag status %d: %s", ordinary.StatusCode, responseText(ordinary))
	}
	var ordinaryJob struct {
		Code string `json:"code"`
		Data struct {
			JobID string `json:"jobId"`
		} `json:"data"`
	}
	if err := json.NewDecoder(ordinary.Body).Decode(&ordinaryJob); err != nil {
		t.Fatal(err)
	}
	ordinary.Body.Close()
	if ordinaryJob.Code != "deployment.queued" {
		t.Fatalf("ordinary Deploy outcome=%#v", ordinaryJob)
	}
	waitForDeploymentJob(t, client, origin, cookie, deployment.ID, ordinaryJob.Data.JobID, "noop")
	assertSavedTagResolution(t, app, deployment.ID, deployment.FrozenCommit)
	identityBeforeFailure, err := identity(deployment.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer failRequests.Store(false)
	failRequests.Store(true)
	failed := projectPost(t, client, origin, cookie, "/api/deployments/"+deployment.ID+"/hard-redeploy", `{}`)
	if failed.StatusCode != http.StatusAccepted {
		t.Fatalf("failed-refresh Hard re-deploy queue status %d: %s", failed.StatusCode, responseText(failed))
	}
	var failedJob struct {
		Code string `json:"code"`
		Data struct {
			JobID string `json:"jobId"`
		} `json:"data"`
	}
	if err := json.NewDecoder(failed.Body).Decode(&failedJob); err != nil {
		t.Fatal(err)
	}
	failed.Body.Close()
	if failedJob.Code != "deployment.hard_redeploy_queued" {
		t.Fatalf("failed Hard re-deploy acceptance outcome=%#v", failedJob)
	}
	waitForDeploymentJob(t, client, origin, cookie, deployment.ID, failedJob.Data.JobID, "failed")
	failRequests.Store(false)
	if after, identityErr := identity(deployment.Path); identityErr != nil || after != identityBeforeFailure {
		t.Fatalf("failed tag refresh changed live directory identity: before=%+v after=%+v err=%v", identityBeforeFailure, after, identityErr)
	}
	assertSavedTagResolution(t, app, deployment.ID, deployment.FrozenCommit)
	hardJob := hardRedeployAndWait(t, client, origin, cookie, deployment.ID, deployment.Path, nil)
	assertSavedTagResolution(t, app, deployment.ID, newCommit)
	if published, readErr := os.ReadFile(filepath.Join(deployment.Path, "README.md")); readErr != nil || string(published) != string(contents)+"\n" {
		t.Fatalf("Hard re-deploy did not publish moved annotated tag: %v", readErr)
	}
	return hardJob
}

func assertSavedTagResolution(t *testing.T, app *App, deploymentID, want string) {
	t.Helper()
	var got string
	if err := app.db.QueryRow(`SELECT commit_id FROM deployments WHERE id=?`, deploymentID).Scan(&got); err != nil || got != want {
		t.Fatalf("saved tag resolution=%q, want %q: %v", got, want, err)
	}
}

func assertTagDeploymentRestarted(t *testing.T, client *http.Client, origin, cookie string, deployments []deployment, tag tagDeploymentFixture, hardJobID, wantCommit string, liveIdentity dirIdentity) {
	t.Helper()
	persisted := false
	for _, item := range deployments {
		if item.ID == tag.ID && item.TargetKind == "tag" && item.TargetName == "v1.0" && item.Commit == wantCommit {
			persisted = true
		}
	}
	if !persisted {
		t.Fatal("explicitly refreshed tag resolution did not survive application restart")
	}
	request, _ := http.NewRequest(http.MethodGet, origin+"/api/deployments/"+tag.ID+"/jobs", nil)
	request.AddCookie(&http.Cookie{Name: sessionCookie, Value: cookie})
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	var jobsOutcome struct {
		Code string          `json:"code"`
		Data []deploymentJob `json:"data"`
	}
	if err := json.NewDecoder(response.Body).Decode(&jobsOutcome); err != nil {
		response.Body.Close()
		t.Fatal(err)
	}
	response.Body.Close()
	for _, item := range jobsOutcome.Data {
		if item.ID == hardJobID && item.Force && item.Status == "failed" {
			if after, identityErr := identity(tag.Path); identityErr != nil || after != liveIdentity {
				t.Fatalf("restart after interrupted hard re-deploy changed live publication: before=%+v after=%+v err=%v", liveIdentity, after, identityErr)
			}
			return
		}
	}
	t.Fatal("hard re-deploy force intent did not survive application restart")
}
