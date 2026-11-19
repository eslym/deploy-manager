package lib

import (
	"context"
	"log"
	"net/http"

	deploymentmodel "github.com/eslym/deploy-manager/lib/ent/deployment"
	jobmodel "github.com/eslym/deploy-manager/lib/ent/deploymentjob"
)

func (a *App) finishAutomaticFetch(jobID, id string, generation int64, paused bool) {
	status, message := "superseded", "Automatic publication invalidated by a control or target change; existing publication kept"
	if paused {
		status, message = "noop", "Automatic deployment paused; repository updates fetched only"
	}
	tx, err := a.store.Tx(context.Background())
	if err != nil {
		log.Printf("cannot begin completion of automatic deployment job %s: %v", jobID, err)
		return
	}
	defer tx.Rollback()
	now := a.cfg.Now().UnixNano()
	job, err := tx.DeploymentJob.Get(context.Background(), jobID)
	if err != nil {
		return
	}
	logText := (job.Log + "\n" + message)
	if len(logText) > 8192 {
		logText = logText[:8192]
	}
	if _, err = tx.DeploymentJob.Update().Where(jobmodel.IDEQ(jobID), jobmodel.StatusEQ("running")).SetStatus(status).SetLog(logText).SetUpdatedAt(now).Save(context.Background()); err != nil {
		log.Printf("cannot complete automatic deployment job %s: %v", jobID, err)
		return
	}
	// A fetch-only delivery must neither claim an unpublished deployment is live
	// nor erase an error left by a failed manual publication.
	d, err := tx.Deployment.Get(context.Background(), id)
	if err != nil {
		return
	}
	active, err := tx.DeploymentJob.Query().Where(jobmodel.DeploymentIDEQ(id), jobmodel.StatusIn("queued", "running")).Exist(context.Background())
	if err != nil {
		return
	}
	if !active && d.Generation == generation && d.Status != "deleting" && d.Status != "deletion_failed" && d.Status != "recovery_required" {
		settled := "idle"
		if d.Error != "" {
			settled = "failed"
		} else if d.LiveCommit != "" {
			settled = "live"
		}
		if _, err = tx.Deployment.Update().Where(deploymentmodel.IDEQ(id), deploymentmodel.GenerationEQ(generation)).SetStatus(settled).SetUpdatedAt(now).Save(context.Background()); err != nil {
			log.Printf("cannot settle automatic deployment %s: %v", id, err)
			return
		}
	}
	if err = tx.Commit(); err != nil {
		log.Printf("cannot commit completion of automatic deployment job %s: %v", jobID, err)
		return
	}
	a.invalidateJob(id)
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
	var slot *deploymentSlot
	if !pause {
		var ok bool
		slot, ok = a.reserveDeploymentSlot()
		if !ok {
			writeOutcomeError(w, http.StatusServiceUnavailable, "operation.capacity_exceeded", "Work capacity is unavailable. Try again later.")
			return
		}
		defer slot.release()
		var err error
		jobID, err = randomID()
		if err != nil {
			writeOutcomeError(w, http.StatusInternalServerError, "service.internal_error", "Operation could not be completed.")
			return
		}
	}
	tx, err := a.store.Tx(r.Context())
	if err != nil {
		writeOutcomeError(w, http.StatusInternalServerError, "service.internal_error", "Operation could not be completed.")
		return
	}
	defer tx.Rollback()
	d, err := tx.Deployment.Get(r.Context(), id)
	if err != nil {
		writeOutcomeError(w, http.StatusNotFound, "request.not_found", "Resource not found.")
		return
	}
	if d.TargetKind != "branch" || d.Status == "deleting" || d.Status == "deletion_failed" {
		writeOutcomeError(w, http.StatusConflict, "request.conflict", "State changed. Refresh and try again.")
		return
	}
	now := a.cfg.Now().UnixNano()
	if pause {
		changed, updateErr := tx.Deployment.Update().Where(deploymentmodel.IDEQ(id), deploymentmodel.AutomationPausedEQ(0)).SetAutomationPaused(1).AddAutomationEpoch(1).SetUpdatedAt(now).Save(r.Context())
		err = updateErr
		if err == nil && changed == 0 {
			_, err = tx.Deployment.Update().Where(deploymentmodel.IDEQ(id)).SetUpdatedAt(now).Save(r.Context())
		}
	} else {
		if d.AutomationPaused == 0 {
			writeOutcomeError(w, http.StatusConflict, "deployment.already_resumed", "Automatic deployment is already enabled.")
			return
		}
		var changed int
		changed, err = tx.Deployment.Update().Where(deploymentmodel.IDEQ(id), deploymentmodel.AutomationPausedEQ(1)).SetAutomationPaused(0).AddAutomationEpoch(1).SetStatus("queued").SetError("").SetUpdatedAt(now).Save(r.Context())
		if err == nil && changed != 1 {
			writeOutcomeError(w, http.StatusConflict, "deployment.already_resumed", "Automatic deployment is already enabled.")
			return
		}
		if err == nil {
			_, err = tx.DeploymentJob.Create().SetID(jobID).SetDeploymentID(id).SetCommitID(d.CommitID).SetGeneration(d.Generation).SetStatus("queued").SetLog("Automatic deployment resumed; latest branch tip queued").SetCreatedAt(now).SetUpdatedAt(now).SetDurable(1).Save(r.Context())
		}
	}
	if err != nil {
		writeOutcomeError(w, http.StatusInternalServerError, "service.internal_error", "Operation could not be completed.")
		return
	}
	if err = tx.Commit(); err != nil {
		writeOutcomeError(w, http.StatusInternalServerError, "service.internal_error", "Operation could not be completed.")
		return
	}
	if pause {
		a.invalidateDeployment(id)
	} else {
		a.invalidateJob(id)
	}
	if pause {
		writeOutcome(w, http.StatusOK, "deployment.paused", "Automatic deployment paused.", map[string]any{"automationPaused": true})
		return
	}
	slot.enqueue(jobID)
	writeOutcome(w, http.StatusAccepted, "deployment.resumed", "Automatic deployment resumed. Publication queued.", map[string]any{"automationPaused": false, "jobId": jobID, "status": "queued"})
}
