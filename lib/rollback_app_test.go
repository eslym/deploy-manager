package lib

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
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
		Commits []struct{ ID, ShortID, Subject, Author, Date string }
		HasMore bool
	}
	if err := json.NewDecoder(response.Body).Decode(&history); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if len(history.Commits) != 1 || !history.HasMore || len(history.Commits[0].ID) != 40 || history.Commits[0].Author == "" || history.Commits[0].Date == "" {
		t.Fatalf("incomplete history: %+v", history)
	}
	firstID := history.Commits[0].ID
	response = get("?page=2&limit=1")
	if err := json.NewDecoder(response.Body).Decode(&history); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if len(history.Commits) != 1 || history.Commits[0].ID == firstID {
		t.Fatalf("pagination repeated first page: %+v", history)
	}
	response = get("?search=" + oldCommit + "&limit=1")
	if err := json.NewDecoder(response.Body).Decode(&history); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if len(history.Commits) != 1 || history.Commits[0].ID != oldCommit || history.HasMore {
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
	var queued struct{ JobID string }
	json.NewDecoder(rollback.Body).Decode(&queued)
	rollback.Body.Close()
	waitForDeploymentJob(t, client, origin, cookie, id, queued.JobID, "succeeded")
	if content, err := os.ReadFile(filepath.Join(path, "README.md")); err != nil || string(content) != "searchable feature\n" {
		t.Fatalf("rollback content=%q err=%v", content, err)
	}
	deploy := projectPost(t, client, origin, cookie, "/api/deployments/"+id+"/deploy", `{}`)
	json.NewDecoder(deploy.Body).Decode(&queued)
	deploy.Body.Close()
	waitForDeploymentJob(t, client, origin, cookie, id, queued.JobID, "succeeded")
	if content, err := os.ReadFile(filepath.Join(path, "README.md")); err != nil || string(content) != "race branch tip\n" {
		t.Fatalf("Deploy did not preserve branch: %q %v", content, err)
	}
}

func testRollbackGraphAndRaces(t *testing.T, client *http.Client, origin, cookie, projectID, id, oldCommit, path string, repo *git.Repository) {
	t.Helper()
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
		var queued struct{ JobID string }
		if err := json.NewDecoder(response.Body).Decode(&queued); err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		return queued.JobID
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
	var hidden struct{ Commits []rollbackCommit }
	if err = json.NewDecoder(response.Body).Decode(&hidden); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if len(hidden.Commits) != 0 {
		t.Fatal("first-parent history displayed second-parent candidate")
	}
	job = queue("rollback", side.String())
	waitForDeploymentJob(t, client, origin, cookie, id, job, "succeeded")
	assertContent("searchable feature\n")
	// Idle completion must not enqueue an unsolicited catch-up.
	time.Sleep(150 * time.Millisecond)
	assertContent("searchable feature\n")
	if err := os.WriteFile(filepath.Join(path, "README.md"), []byte("damaged live files\n"), 0600); err != nil {
		t.Fatal(err)
	}
	job = queue("rollback", side.String())
	waitForDeploymentJob(t, client, origin, cookie, id, job, "succeeded")
	assertContent("searchable feature\n")
	job = queue("deploy", "")
	waitForDeploymentJob(t, client, origin, cookie, id, job, "succeeded")
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
		job = queue("rollback", side.String())
		close(release)
		waitForDeploymentJob(t, client, origin, cookie, id, stale, "superseded")
		waitForDeploymentJob(t, client, origin, cookie, id, job, "succeeded")
		setPublicationFaultPoint(previous)
		assertContent("searchable feature\n")
	}
	// Put the common fixture back for the rest of its lifecycle scenarios.
	// A Git-readable ancestor with unavailable LFS material cannot replace live.
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
	var queued struct{ JobID string }
	if err = json.NewDecoder(response.Body).Decode(&queued); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	waitForDeploymentJob(t, client, origin, cookie, id, queued.JobID, "failed")
	if after, err := identity(path); err != nil || after != before {
		t.Fatalf("failed rollback changed live directory: %+v -> %+v: %v", before, after, err)
	}
}
