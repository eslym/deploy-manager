package lib

import (
	"log"
	"net/http"
)

func (a *App) finishAutomaticFetch(jobID, id string, generation int64, paused bool) {
	status, message := "superseded", "Automatic publication invalidated by a control or target change; existing publication kept"
	if paused {
		status, message = "noop", "Automatic deployment paused; repository updates fetched only"
	}
	tx, err := a.db.Begin()
	if err != nil {
		log.Printf("cannot begin completion of automatic deployment job %s: %v", jobID, err)
		return
	}
	defer tx.Rollback()
	now := a.cfg.Now().UnixNano()
	if _, err = tx.Exec(`UPDATE deployment_jobs SET status=?,log=substr(log || char(10) || ?,1,8192),updated_at=? WHERE id=? AND status='running'`, status, message, now, jobID); err != nil {
		log.Printf("cannot complete automatic deployment job %s: %v", jobID, err)
		return
	}
	// A fetch-only delivery must neither claim an unpublished deployment is live
	// nor erase an error left by a failed manual publication.
	if _, err = tx.Exec(`UPDATE deployments SET status=CASE WHEN error<>'' THEN 'failed' WHEN live_commit<>'' THEN 'live' ELSE 'idle' END,updated_at=? WHERE id=? AND generation=? AND status NOT IN ('deleting','deletion_failed','recovery_required') AND NOT EXISTS (SELECT 1 FROM deployment_jobs WHERE deployment_id=? AND status IN ('queued','running'))`, now, id, generation, id); err != nil {
		log.Printf("cannot settle automatic deployment %s: %v", id, err)
		return
	}
	if err = tx.Commit(); err != nil {
		log.Printf("cannot commit completion of automatic deployment job %s: %v", jobID, err)
	}
}

// Automation has its own revision: pausing invalidates accepted automatic work
// without cancelling a deliberately requested one-off manual publication.
func (a *App) initDeploymentAutomation() error {
	for _, column := range []struct{ name, definition string }{
		{"automation_paused", `INTEGER NOT NULL DEFAULT 0`},
		{"automation_epoch", `INTEGER NOT NULL DEFAULT 0`},
	} {
		if err := ensureColumn(a.db, "deployments", column.name, column.definition); err != nil {
			return err
		}
	}
	var exists int
	if err := a.db.QueryRow(`SELECT count(*) FROM pragma_table_info('deployment_jobs') WHERE name='automatic'`).Scan(&exists); err != nil {
		return err
	}
	if err := ensureColumn(a.db, "deployment_jobs", "automation_epoch", `INTEGER NOT NULL DEFAULT 0`); err != nil {
		return err
	}
	// Before explicit job provenance existed, durable jobs were exclusively
	// verified webhooks. Migrate once; newer durable manual jobs stay manual.
	if exists == 0 {
		tx, err := a.db.Begin()
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if _, err = tx.Exec(`ALTER TABLE deployment_jobs ADD COLUMN automatic INTEGER NOT NULL DEFAULT 0`); err != nil {
			return err
		}
		if _, err = tx.Exec(`UPDATE deployment_jobs SET automatic=1 WHERE durable=1 AND rollback_commit=''`); err != nil {
			return err
		}
		return tx.Commit()
	}
	return nil
}

func (a *App) pauseDeployment(w http.ResponseWriter, r *http.Request) {
	a.controlDeploymentAutomation(w, r, true)
}

func (a *App) resumeDeployment(w http.ResponseWriter, r *http.Request) {
	a.controlDeploymentAutomation(w, r, false)
}

func (a *App) controlDeploymentAutomation(w http.ResponseWriter, r *http.Request, pause bool) {
	if !a.projectAuthorized(w, r, true) {
		return
	}
	id := r.PathValue("id")
	jobID := ""
	reserved := false
	if !pause {
		select {
		case a.deploySlots <- struct{}{}:
			reserved = true
		default:
			http.Error(w, "the publication queue is full; try again shortly", http.StatusServiceUnavailable)
			return
		}
		defer func() {
			if reserved {
				<-a.deploySlots
			}
		}()
		var err error
		jobID, err = randomID()
		if err != nil {
			http.Error(w, "publication could not be queued", 500)
			return
		}
	}
	tx, err := a.db.Begin()
	if err != nil {
		http.Error(w, "automatic deployment could not be updated", 500)
		return
	}
	defer tx.Rollback()
	var kind, status, commit string
	var generation int64
	var paused bool
	if err = tx.QueryRow(`SELECT target_kind,status,commit_id,generation,automation_paused FROM deployments WHERE id=?`, id).Scan(&kind, &status, &commit, &generation, &paused); err != nil {
		http.NotFound(w, r)
		return
	}
	if kind != "branch" || status == "deleting" || status == "deletion_failed" {
		http.Error(w, "automatic deployment is available only for active branch targets", http.StatusConflict)
		return
	}
	now := a.cfg.Now().UnixNano()
	if pause {
		_, err = tx.Exec(`UPDATE deployments SET automation_paused=1,automation_epoch=automation_epoch+CASE WHEN automation_paused=0 THEN 1 ELSE 0 END,updated_at=? WHERE id=?`, now, id)
	} else {
		if !paused {
			http.Error(w, "automatic deployment is already enabled", http.StatusConflict)
			return
		}
		_, err = tx.Exec(`UPDATE deployments SET automation_paused=0,automation_epoch=automation_epoch+1,status='queued',error='',updated_at=? WHERE id=?`, now, id)
		if err == nil {
			_, err = tx.Exec(`INSERT INTO deployment_jobs(id,deployment_id,commit_id,generation,status,log,created_at,updated_at,durable) VALUES(?,?,?,?,'queued','Automatic deployment resumed; latest branch tip queued',?,?,1)`, jobID, id, commit, generation, now, now)
		}
	}
	if err != nil {
		http.Error(w, "automatic deployment could not be updated", 500)
		return
	}
	if err = tx.Commit(); err != nil {
		http.Error(w, "automatic deployment could not be updated", 500)
		return
	}
	if pause {
		writeJSON(w, map[string]any{"automationPaused": true})
		return
	}
	a.deployQueue <- jobID
	reserved = false
	writeJSONStatus(w, http.StatusAccepted, map[string]any{"automationPaused": false, "jobId": jobID, "status": "queued"})
}
