package lib

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

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
			time.Sleep(150 * time.Millisecond)
			if jobs := f.waitJobs(t, id); len(jobs) != len(beforeJobs) || f.gitFetches.Load() != beforeFetches {
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
