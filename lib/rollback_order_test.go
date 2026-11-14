package lib

import (
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestLaterWebhookCannotOvertakeAcceptedRollback(t *testing.T) {
	forAutomationProviders(t, func(t *testing.T, provider string) {
		f := newAutomationFixture(t, provider)
		id := f.createBranch(t, "ordered-rollback")
		latest := addSmartFixtureCommit(t, f.remoteDir, "latest branch tip")
		job := f.action(t, id, "deploy", `{}`)
		waitForDeploymentJob(t, f.server.Client(), f.server.URL, f.cookie, id, job, "succeeded")
		ready, release := make(chan struct{}), make(chan struct{})
		var first atomic.Bool
		var unblock sync.Once
		previous := setPublicationFaultPoint(func(point string) error {
			if point == "before-job-lock" && first.CompareAndSwap(false, true) {
				close(ready)
				<-release
			}
			return nil
		})
		defer setPublicationFaultPoint(previous)
		defer unblock.Do(func() { close(release) })
		rollback := f.action(t, id, "rollback", `{"commit":"`+f.initialCommit+`"}`)
		select {
		case <-ready:
		case <-time.After(5 * time.Second):
			t.Fatal("rollback did not reach deployment lock")
		}
		f.push(t, "later-push-before-rollback-lock", latest)
		// The rollback worker remains blocked, so only the later webhook worker
		// can take the lock and settle these jobs. It must run rollback first.
		jobs := f.waitJobs(t, id)
		if jobs[0].Status != "succeeded" || jobs[0].Commit != latest {
			t.Fatalf("later webhook did not publish the tip after rollback: %+v", jobs[0])
		}
		unblock.Do(func() { close(release) })
		waitForDeploymentJob(t, f.server.Client(), f.server.URL, f.cookie, id, rollback, "succeeded")
		// Closing waits for the original worker too: it must not run rollback a
		// second time after the later webhook has completed.
		f.server.Close()
		if err := f.app.Close(); err != nil {
			t.Fatal(err)
		}
		content, err := os.ReadFile(filepath.Join(f.app.deploymentPath(id), "README.md"))
		if err != nil || string(content) != "latest branch tip\n" {
			t.Fatalf("later accepted webhook lost to rollback: %q %v", content, err)
		}
	})
}
