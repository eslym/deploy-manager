package lib

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
)

func testTemporaryRollback(t *testing.T, client *http.Client, origin, cookie, id, oldCommit, path string) {
	t.Helper()
	unauthenticated, err := client.Get(origin + "/api/deployments/" + id + "/history")
	if err != nil {
		t.Fatal(err)
	}
	if unauthenticated.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unprotected rollback history: %d", unauthenticated.StatusCode)
	}
	unauthenticated.Body.Close()
	unsafe, _ := http.NewRequest(http.MethodPost, origin+"/api/deployments/"+id+"/rollback", strings.NewReader(`{"commit":"`+oldCommit+`"}`))
	unsafe.AddCookie(&http.Cookie{Name: sessionCookie, Value: cookie})
	unsafe.Header.Set("Content-Type", "application/json")
	unsafeResponse, err := client.Do(unsafe)
	if err != nil {
		t.Fatal(err)
	}
	if unsafeResponse.StatusCode != http.StatusForbidden {
		t.Fatalf("rollback accepted without Origin: %d", unsafeResponse.StatusCode)
	}
	unsafeResponse.Body.Close()
	get := func(query string) *http.Response {
		r, _ := http.NewRequest("GET", origin+"/api/deployments/"+id+"/history"+query, nil)
		r.AddCookie(&http.Cookie{Name: sessionCookie, Value: cookie})
		response, err := client.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		return response
	}
	response := get("?limit=1")
	if response.StatusCode != 200 {
		t.Fatalf("history status %d: %s", response.StatusCode, responseText(response))
	}
	var history struct {
		Code string `json:"code"`
		Data struct {
			Commits []struct {
				ID      string `json:"id"`
				ShortID string `json:"shortId"`
				Subject string `json:"subject"`
				Author  string `json:"author"`
				Date    string `json:"date"`
			} `json:"commits"`
			Page    int  `json:"page"`
			HasMore bool `json:"hasMore"`
		} `json:"data"`
	}
	if err := json.NewDecoder(response.Body).Decode(&history); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK || history.Code != "deployment.history_loaded" || len(history.Data.Commits) != 1 || !history.Data.HasMore || len(history.Data.Commits[0].ID) != 40 || history.Data.Commits[0].Author == "" || history.Data.Commits[0].Date == "" {
		t.Fatalf("incomplete history: %+v", history)
	}
	firstID := history.Data.Commits[0].ID
	response = get("?page=2&limit=1")
	if err := json.NewDecoder(response.Body).Decode(&history); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if len(history.Data.Commits) != 1 || history.Data.Page != 2 || history.Data.Commits[0].ID == firstID {
		t.Fatalf("pagination repeated first page: %+v", history)
	}
	response = get("?search=" + oldCommit + "&limit=1")
	if err := json.NewDecoder(response.Body).Decode(&history); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if len(history.Data.Commits) != 1 || history.Data.Commits[0].ID != oldCommit || history.Data.HasMore {
		t.Fatalf("search history: %+v", history)
	}
	rejected := projectPost(t, client, origin, cookie, "/api/deployments/"+id+"/rollback", `{"commit":"`+oldCommit[:12]+`"}`)
	if rejected.StatusCode != 400 {
		t.Fatalf("abbreviated action accepted: %d", rejected.StatusCode)
	}
	rejected.Body.Close()
	rollback := projectPost(t, client, origin, cookie, "/api/deployments/"+id+"/rollback", `{"commit":"`+oldCommit+`"}`)
	if rollback.StatusCode != 202 {
		t.Fatalf("rollback status %d: %s", rollback.StatusCode, responseText(rollback))
	}
	var queued struct {
		Code string `json:"code"`
		Data struct {
			JobID string `json:"jobId"`
		} `json:"data"`
	}
	json.NewDecoder(rollback.Body).Decode(&queued)
	rollback.Body.Close()
	if queued.Code != "deployment.rollback_queued" || queued.Data.JobID == "" {
		t.Fatalf("rollback outcome=%#v", queued)
	}
	waitForDeploymentJob(t, client, origin, cookie, id, queued.Data.JobID, "succeeded")
	if content, err := os.ReadFile(filepath.Join(path, "README.md")); err != nil || string(content) != "searchable feature\n" {
		t.Fatalf("rollback content=%q err=%v", content, err)
	}
	deploy := projectPost(t, client, origin, cookie, "/api/deployments/"+id+"/deploy", `{}`)
	json.NewDecoder(deploy.Body).Decode(&queued)
	deploy.Body.Close()
	if queued.Code != "deployment.queued" || queued.Data.JobID == "" {
		t.Fatalf("deploy outcome=%#v", queued)
	}
	waitForDeploymentJob(t, client, origin, cookie, id, queued.Data.JobID, "succeeded")
	if content, err := os.ReadFile(filepath.Join(path, "README.md")); err != nil || string(content) != "race branch tip\n" {
		t.Fatalf("Deploy did not preserve branch: %q %v", content, err)
	}
}

func testRollbackGraphAndRaces(t *testing.T, server *httptest.Server, cookie, projectID, id, oldCommit, path string, repo *git.Repository) {
	t.Helper()
	client, origin := server.Client(), server.URL
	branch := plumbing.NewBranchReferenceName("feature/search")
	ref, err := repo.Reference(branch, true)
	if err != nil {
		t.Fatal(err)
	}
	tip := ref.Hash()
	old, err := repo.CommitObject(plumbing.NewHash(oldCommit))
	if err != nil {
		t.Fatal(err)
	}
	tipCommit, err := repo.CommitObject(tip)
	if err != nil {
		t.Fatal(err)
	}
	makeCommit := func(tree plumbing.Hash, subject string, parents ...plumbing.Hash) plumbing.Hash {
		sig := object.Signature{Name: "Rollback fixture", Email: "rollback@example.test", When: time.Now().UTC()}
		commit := object.Commit{Author: sig, Committer: sig, TreeHash: tree, ParentHashes: parents, Message: subject}
		encoded := repo.Storer.NewEncodedObject()
		if err := commit.Encode(encoded); err != nil {
			t.Fatal(err)
		}
		hash, err := repo.Storer.SetEncodedObject(encoded)
		if err != nil {
			t.Fatal(err)
		}
		return hash
	}
	side := makeCommit(old.TreeHash, "second-parent-only candidate", old.Hash)
	merge := makeCommit(tipCommit.TreeHash, "merged rollback candidate", tip, side)
	setTip := func(hash plumbing.Hash) {
		if err := repo.Storer.SetReference(plumbing.NewHashReference(branch, hash)); err != nil {
			t.Fatal(err)
		}
	}
	queue := func(action, commit string) string {
		body := `{}`
		if commit != "" {
			body = `{"commit":"` + commit + `"}`
		}
		response := projectPost(t, client, origin, cookie, "/api/deployments/"+id+"/"+action, body)
		if response.StatusCode != 202 {
			t.Fatalf("%s status %d: %s", action, response.StatusCode, responseText(response))
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
		wantCode := "deployment.queued"
		if action == "rollback" {
			wantCode = "deployment.rollback_queued"
		} else if action == "hard-redeploy" {
			wantCode = "deployment.hard_redeploy_queued"
		}
		if queued.Code != wantCode || queued.Data.JobID == "" {
			t.Fatalf("%s outcome=%#v", action, queued)
		}
		return queued.Data.JobID
	}
	assertContent := func(want string) {
		t.Helper()
		content, err := os.ReadFile(filepath.Join(path, "README.md"))
		if err != nil || string(content) != want {
			t.Fatalf("live content %q want %q: %v", content, want, err)
		}
	}
	setTip(merge)
	job := queue("deploy", "")
	waitForDeploymentJob(t, client, origin, cookie, id, job, "succeeded")
	request, _ := http.NewRequest("GET", origin+"/api/deployments/"+id+"/history?search="+side.String(), nil)
	request.AddCookie(&http.Cookie{Name: sessionCookie, Value: cookie})
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	var hidden struct {
		Code string `json:"code"`
		Data struct {
			Commits []rollbackCommit `json:"commits"`
			Page    int              `json:"page"`
			HasMore bool             `json:"hasMore"`
		} `json:"data"`
	}
	if err = json.NewDecoder(response.Body).Decode(&hidden); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK || hidden.Code != "deployment.history_loaded" || len(hidden.Data.Commits) != 0 {
		t.Fatal("first-parent history displayed second-parent candidate")
	}
	expectedJobs := make(map[string]bool)
	for _, existing := range publicDeploymentJobs(t, server, cookie, id) {
		expectedJobs[existing.ID] = true
	}
	job = queue("rollback", side.String())
	expectedJobs[job] = true
	waitForDeploymentJob(t, client, origin, cookie, id, job, "succeeded")
	assertContent("searchable feature\n")
	if err := os.WriteFile(filepath.Join(path, "README.md"), []byte("damaged live files\n"), 0600); err != nil {
		t.Fatal(err)
	}
	job = queue("rollback", side.String())
	expectedJobs[job] = true
	waitForDeploymentJob(t, client, origin, cookie, id, job, "succeeded")
	assertContent("searchable feature\n")
	job = queue("deploy", "")
	expectedJobs[job] = true
	waitForDeploymentJob(t, client, origin, cookie, id, job, "succeeded")
	// Completing the next explicit publication is a progress barrier. Check
	// durable history, not an arbitrary quiet period: even a superseded catch-up
	// would leave a job that was never explicitly requested here.
	actualJobs := publicDeploymentJobs(t, server, cookie, id)
	if len(actualJobs) != len(expectedJobs) {
		t.Fatalf("rollback queued unsolicited work: got %d jobs, want %d", len(actualJobs), len(expectedJobs))
	}
	for _, actual := range actualJobs {
		if !expectedJobs[actual.ID] {
			t.Fatalf("rollback queued unsolicited job %s (%s)", actual.ID, actual.Status)
		}
	}
	// Force push and successfully fetch a new graph while rollback is staged.
	var once sync.Once
	previous := setPublicationFaultPoint(func(point string) error {
		if point == "after-stage-fsync" {
			once.Do(func() {
				setTip(tip)
				refreshed := projectPost(t, client, origin, cookie, "/api/projects/"+projectID+"/refresh", `{}`)
				if refreshed.StatusCode != 200 {
					t.Errorf("race refresh status %d: %s", refreshed.StatusCode, responseText(refreshed))
				}
				refreshed.Body.Close()
			})
		}
		return nil
	})
	job = queue("rollback", side.String())
	waitForDeploymentJob(t, client, origin, cookie, id, job, "failed")
	setPublicationFaultPoint(previous)
	assertContent("race branch tip\n")
	// A selection which remains cached but is not in the fetched graph fails too.
	job = queue("rollback", side.String())
	waitForDeploymentJob(t, client, origin, cookie, id, job, "failed")
	assertContent("race branch tip\n")
	job = queue("rollback", strings.Repeat("f", 40))
	waitForDeploymentJob(t, client, origin, cookie, id, job, "failed")
	assertContent("race branch tip\n")
	// An older prepared publication must not overwrite an accepted rollback.
	setTip(merge)
	for _, boundary := range []string{"after-stage-fsync", "after-intent"} {
		staged := make(chan struct{})
		release := make(chan struct{})
		var block sync.Once
		previous = setPublicationFaultPoint(func(point string) error {
			if point == boundary {
				block.Do(func() { close(staged); <-release })
			}
			return nil
		})
		stale := queue("hard-redeploy", "")
		select {
		case <-staged:
		case <-time.After(5 * time.Second):
			t.Fatal("publication did not stage")
		}
		olderPending := []string{queue("deploy", ""), queue("deploy", "")}
		job = queue("rollback", side.String())
		close(release)
		waitForDeploymentJob(t, client, origin, cookie, id, stale, "superseded")
		for _, pending := range olderPending {
			waitForDeploymentJob(t, client, origin, cookie, id, pending, "superseded")
		}
		waitForDeploymentJob(t, client, origin, cookie, id, job, "succeeded")
		setPublicationFaultPoint(previous)
		assertContent("searchable feature\n")
	}
	// Put the common fixture back for the rest of its lifecycle scenarios.
	// A Git-readable ancestor with unavailable LFS material cannot replace live.
	// Later manual tip work accepted while rollback is still preparing must use
	// the live state after taking the per-deployment lock, not its old snapshot.
	setTip(merge)
	job = queue("deploy", "")
	waitForDeploymentJob(t, client, origin, cookie, id, job, "succeeded")
	rollbackStaged := make(chan struct{})
	releaseRollback := make(chan struct{})
	laterReady := make(chan struct{})
	var staging, ready sync.Once
	var later atomic.Bool
	previous = setPublicationFaultPoint(func(point string) error {
		if point == "after-stage-fsync" {
			staging.Do(func() { close(rollbackStaged); <-releaseRollback })
		}
		if point == "before-job-lock" && later.Load() {
			ready.Do(func() { close(laterReady) })
		}
		return nil
	})
	rollbackJob := queue("rollback", side.String())
	select {
	case <-rollbackStaged:
	case <-time.After(5 * time.Second):
		t.Fatal("rollback did not stage")
	}
	later.Store(true)
	laterJob := queue("deploy", "")
	select {
	case <-laterReady:
	case <-time.After(5 * time.Second):
		t.Fatal("later Deploy did not reach deployment lock")
	}
	close(releaseRollback)
	waitForDeploymentJob(t, client, origin, cookie, id, rollbackJob, "succeeded")
	waitForDeploymentJob(t, client, origin, cookie, id, laterJob, "succeeded")
	setPublicationFaultPoint(previous)
	assertContent("race branch tip\n")
	// Dequeue order is not lock-acquisition order. Hold the accepted rollback
	// before its lock, then let a later Deploy acquire the lock first. Both jobs
	// must finish in acceptance order, even though the tip is already live.
	rollbackReady := make(chan struct{})
	releaseBeforeLock := make(chan struct{})
	var releaseLock sync.Once
	defer releaseLock.Do(func() { close(releaseBeforeLock) })
	var firstLock atomic.Bool
	previous = setPublicationFaultPoint(func(point string) error {
		if point == "before-job-lock" && firstLock.CompareAndSwap(false, true) {
			close(rollbackReady)
			<-releaseBeforeLock
		}
		return nil
	})
	rollbackJob = queue("rollback", side.String())
	select {
	case <-rollbackReady:
	case <-time.After(5 * time.Second):
		t.Fatal("rollback did not reach deployment lock")
	}
	laterJob = queue("deploy", "")
	// Do not release the rollback worker until the later worker has completed:
	// this deterministically schedules the later job's lock ahead of rollback.
	waitForDeploymentJob(t, client, origin, cookie, id, laterJob, "succeeded")
	releaseLock.Do(func() { close(releaseBeforeLock) })
	waitForDeploymentJob(t, client, origin, cookie, id, rollbackJob, "succeeded")
	setPublicationFaultPoint(previous)
	assertContent("race branch tip\n")
	job = queue("rollback", side.String())
	waitForDeploymentJob(t, client, origin, cookie, id, job, "succeeded")
	storeBlob := func(contents string) plumbing.Hash {
		encoded := repo.Storer.NewEncodedObject()
		encoded.SetType(plumbing.BlobObject)
		writer, err := encoded.Writer()
		if err != nil {
			t.Fatal(err)
		}
		if _, err = writer.Write([]byte(contents)); err != nil {
			t.Fatal(err)
		}
		if err = writer.Close(); err != nil {
			t.Fatal(err)
		}
		hash, err := repo.Storer.SetEncodedObject(encoded)
		if err != nil {
			t.Fatal(err)
		}
		return hash
	}
	oldTree, err := old.Tree()
	if err != nil {
		t.Fatal(err)
	}
	tree := object.Tree{Entries: append([]object.TreeEntry{}, oldTree.Entries...)}
	tree.Entries = append(tree.Entries, object.TreeEntry{Name: ".gitattributes", Mode: filemode.Regular, Hash: storeBlob("*.dat filter=lfs\n")}, object.TreeEntry{Name: "asset.dat", Mode: filemode.Regular, Hash: storeBlob("version https://git-lfs.github.com/spec/v1\noid sha256:" + strings.Repeat("0", 64) + "\nsize 1\n")})
	encoded := repo.Storer.NewEncodedObject()
	sort.Slice(tree.Entries, func(i, j int) bool { return tree.Entries[i].Name < tree.Entries[j].Name })
	if err = tree.Encode(encoded); err != nil {
		t.Fatal(err)
	}
	treeHash, err := repo.Storer.SetEncodedObject(encoded)
	if err != nil {
		t.Fatal(err)
	}
	lfs := makeCommit(treeHash, "unavailable old LFS material", old.Hash)
	lfsMerge := makeCommit(tipCommit.TreeHash, "merge LFS ancestor", tip, lfs)
	setTip(lfsMerge)
	job = queue("rollback", lfs.String())
	waitForDeploymentJob(t, client, origin, cookie, id, job, "failed")
	assertContent("searchable feature\n")
	setTip(tip)
	job = queue("deploy", "")
	waitForDeploymentJob(t, client, origin, cookie, id, job, "succeeded")
	assertContent("race branch tip\n")
	hardRedeployAndWait(t, client, origin, cookie, id, path, nil)
	testTemporaryRollbackPreservesPause(t, client, origin, cookie, projectID, id, oldCommit, path)
}

func testTemporaryRollbackPreservesPause(t *testing.T, client *http.Client, origin, cookie, projectID, id, commit, path string) {
	t.Helper()
	pause := projectPost(t, client, origin, cookie, "/api/deployments/"+id+"/pause", `{}`)
	if pause.StatusCode != 200 {
		t.Fatalf("pause before rollback status %d: %s", pause.StatusCode, responseText(pause))
	}
	var paused struct {
		Code string `json:"code"`
		Data struct {
			AutomationPaused bool `json:"automationPaused"`
		} `json:"data"`
	}
	if err := json.NewDecoder(pause.Body).Decode(&paused); err != nil || paused.Code != "deployment.paused" || !paused.Data.AutomationPaused {
		t.Fatalf("pause outcome=%#v, err=%v", paused, err)
	}
	pause.Body.Close()
	assertPaused := func() {
		request, _ := http.NewRequest("GET", origin+"/api/deployments", nil)
		request.AddCookie(&http.Cookie{Name: sessionCookie, Value: cookie})
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		var outcome struct {
			Code string       `json:"code"`
			Data []deployment `json:"data"`
		}
		if err = json.NewDecoder(response.Body).Decode(&outcome); err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		for _, item := range outcome.Data {
			if item.ID == id {
				if !item.AutomationPaused || item.TargetKind != "branch" || item.TargetName != "feature/search" {
					t.Fatalf("temporary rollback changed branch automation: %+v", item)
				}
				return
			}
		}
		t.Fatal("deployment missing")
	}
	rollback := projectPost(t, client, origin, cookie, "/api/deployments/"+id+"/rollback", `{"commit":"`+commit+`"}`)
	var queued struct {
		Code string `json:"code"`
		Data struct {
			JobID string `json:"jobId"`
		} `json:"data"`
	}
	if rollback.StatusCode != 202 || json.NewDecoder(rollback.Body).Decode(&queued) != nil {
		t.Fatalf("paused rollback status %d", rollback.StatusCode)
	}
	rollback.Body.Close()
	if queued.Code != "deployment.rollback_queued" {
		t.Fatalf("paused rollback outcome=%#v", queued)
	}
	waitForDeploymentJob(t, client, origin, cookie, id, queued.Data.JobID, "succeeded")
	assertPaused()
	for _, provider := range []string{"github", "gitea"} {
		response := projectPost(t, client, origin, cookie, "/api/projects/"+projectID+"/"+provider+"-webhook", `{"rotate":true}`)
		var settings struct {
			Code string `json:"code"`
			Data struct {
				URL    string `json:"url"`
				Secret string `json:"secret"`
			} `json:"data"`
		}
		if response.StatusCode != 201 || json.NewDecoder(response.Body).Decode(&settings) != nil {
			t.Fatalf("paused rollback webhook settings %d", response.StatusCode)
		}
		if settings.Code != "webhook.secret_rotated" {
			t.Fatalf("paused rollback webhook outcome %q", settings.Code)
		}
		response.Body.Close()
		body := []byte(`{"ref":"refs/heads/feature/search","after":"` + commit + `"}`)
		send := sendGitHubWebhook
		if provider == "gitea" {
			send = sendGiteaWebhook
		}
		if status := send(client, origin+settings.Data.URL, "paused-temporary-rollback-"+provider, "push", body, []byte(settings.Data.Secret)); status != 200 {
			t.Fatalf("paused %s webhook status %d", provider, status)
		}
		deadline := time.Now().Add(10 * time.Second)
		for {
			request, _ := http.NewRequest("GET", origin+"/api/deployments/"+id+"/jobs", nil)
			request.AddCookie(&http.Cookie{Name: sessionCookie, Value: cookie})
			response, err := client.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			var outcome struct {
				Code string          `json:"code"`
				Data []deploymentJob `json:"data"`
			}
			err = json.NewDecoder(response.Body).Decode(&outcome)
			response.Body.Close()
			if err != nil {
				t.Fatal(err)
			}
			if len(outcome.Data) > 0 && outcome.Data[0].ID != queued.Data.JobID && outcome.Data[0].Status == "noop" {
				queued.Data.JobID = outcome.Data[0].ID
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("paused %s webhook did not finish fetch-only: %+v", provider, outcome.Data)
			}
			time.Sleep(10 * time.Millisecond)
		}
		if content, err := os.ReadFile(filepath.Join(path, "README.md")); err != nil || string(content) != "searchable feature\n" {
			t.Fatalf("paused webhook replaced rollback content %q: %v", content, err)
		}
		assertPaused()
	}
	testRejectedRollbackKeepsLive(t, client, origin, cookie, id, strings.Repeat("f", 40), path)
	assertPaused()
	resume := projectPost(t, client, origin, cookie, "/api/deployments/"+id+"/resume", `{}`)
	if resume.StatusCode != 202 || json.NewDecoder(resume.Body).Decode(&queued) != nil || queued.Code != "deployment.resumed" {
		t.Fatalf("resume after rollback status %d", resume.StatusCode)
	}
	resume.Body.Close()
	waitForDeploymentJob(t, client, origin, cookie, id, queued.Data.JobID, "succeeded")
}

func testRejectedRollbackKeepsLive(t *testing.T, client *http.Client, origin, cookie, id, commit, path string) {
	t.Helper()
	before, err := identity(path)
	if err != nil {
		t.Fatal(err)
	}
	response := projectPost(t, client, origin, cookie, "/api/deployments/"+id+"/rollback", `{"commit":"`+commit+`"}`)
	if response.StatusCode != 202 {
		t.Fatalf("rollback rejection job status %d: %s", response.StatusCode, responseText(response))
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
	if queued.Code != "deployment.rollback_queued" {
		t.Fatalf("rejected rollback acceptance outcome=%#v", queued)
	}
	waitForDeploymentJob(t, client, origin, cookie, id, queued.Data.JobID, "failed")
	if after, err := identity(path); err != nil || after != before {
		t.Fatalf("failed rollback changed live directory: %+v -> %+v: %v", before, after, err)
	}
}
