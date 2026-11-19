package lib

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"

	"github.com/eslym/deploy-manager/lib/ent"
	"golang.org/x/sys/unix"
)

type publicationMove struct {
	id, source, destination string
	old                     dirIdentity
}

func (a *App) movePending(id string) (bool, error) {
	var count int
	err := a.db.QueryRow(`SELECT count(*) FROM publication_moves WHERE deployment_id=?`, id).Scan(&count)
	return count != 0, err
}

// reconcileMove observes both names before changing the durable binding. Never
// repeat a rename: after a crash, the source and destination inode identities
// alone decide whether the move happened.
func (a *App) reconcileMove(id string) error {
	var m publicationMove
	var dev, ino int64
	err := a.db.QueryRow(`SELECT deployment_id,source_name,destination_name,old_dev,old_ino FROM publication_moves WHERE deployment_id=?`, id).Scan(&m.id, &m.source, &m.destination, &dev, &ino)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	m.old = dirIdentity{uint64(dev), uint64(ino)}
	if !validProjectID(id) || !validStoredPublicationName(m.source, id) || !validPublicationName(m.destination) || m.source == m.destination || m.old.Dev == 0 || m.old.Ino == 0 {
		return errors.New("ambiguous publication move journal")
	}
	if err := a.assertPublicationRoot(); err != nil {
		return err
	}
	owner := a.publicationPaths(id)
	source, err := owner.namedPath(m.source)
	if err != nil {
		return err
	}
	destination, err := owner.namedPath(m.destination)
	if err != nil {
		return err
	}
	sourceID, sourceErr := identity(source)
	destinationID, destinationErr := identity(destination)
	if sourceErr != nil && !os.IsNotExist(sourceErr) {
		return fmt.Errorf("ambiguous move source: %w", sourceErr)
	}
	if destinationErr != nil && !os.IsNotExist(destinationErr) {
		return fmt.Errorf("ambiguous move destination: %w", destinationErr)
	}
	before := sourceErr == nil && sourceID == m.old && os.IsNotExist(destinationErr)
	after := os.IsNotExist(sourceErr) && destinationErr == nil && destinationID == m.old
	if !before && !after {
		return errors.New("ambiguous publication move: recorded directory identity or destination changed")
	}
	if after {
		// Recovery can observe a rename that returned before its parent was
		// synced. Durably persist the observed namespace before recording it.
		if err := syncDirectory(a.cfg.DeploymentRoot); err != nil {
			return err
		}
		stillMoved, e := sameIdentity(destination, m.old)
		if e != nil || !stillMoved {
			return errors.New("publication move changed during recovery")
		}
		if _, e = os.Lstat(source); !os.IsNotExist(e) {
			return errors.New("publication move source reappeared during recovery")
		}
	}
	ctx := context.Background()
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var name, liveCommit string
	var recordedDev, recordedIno int64
	if err = tx.QueryRowContext(ctx, `SELECT publication_name,live_commit,live_dev,live_ino FROM deployments WHERE id=?`, id).Scan(&name, &liveCommit, &recordedDev, &recordedIno); err != nil {
		return err
	}
	if liveCommit == "" || (dirIdentity{uint64(recordedDev), uint64(recordedIno)}) != m.old {
		return errors.New("ambiguous publication move: deployment ownership changed")
	}
	if before && name != m.source || after && name != m.source && name != m.destination {
		return errors.New("ambiguous publication move binding")
	}
	if after && name == m.source {
		if _, err = tx.ExecContext(ctx, `UPDATE deployments SET publication_name=?,updated_at=? WHERE id=? AND publication_name=?`, m.destination, a.cfg.Now().UnixNano(), id, m.source); err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM publication_moves WHERE deployment_id=?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

func (a *App) reconcileMoves() error {
	rows, err := a.db.Query(`SELECT deployment_id FROM publication_moves ORDER BY deployment_id`)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			break
		}
		ids = append(ids, id)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err = a.reconcileMove(id); err != nil {
			return err
		}
	}
	return nil
}

func (a *App) moveDeployment(w http.ResponseWriter, r *http.Request) {
	if !a.projectAuthorized(w, r, true) {
		return
	}
	var input struct {
		PublicationPath         string `json:"publicationPath"`
		ExpectedPublicationPath string `json:"expectedPublicationPath"`
		Confirm                 bool   `json:"confirm"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&input) != nil || !input.Confirm || !validPublicationName(input.PublicationPath) {
		http.Error(w, "choose a safe folder name and explicitly confirm the external web server document root may need updating", http.StatusBadRequest)
		return
	}
	id := r.PathValue("id")
	if !validProjectID(id) {
		http.NotFound(w, r)
		return
	}
	unlock, err := a.lockDeployment(id)
	if err != nil {
		http.Error(w, "publication root is unavailable", http.StatusServiceUnavailable)
		return
	}
	defer unlock()
	ctx := context.Background() // A disconnected client cannot cancel a durable filesystem move.
	current, err := a.store.Deployment.Get(ctx, id)
	if ent.IsNotFound(err) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "deployment unavailable", 503)
		return
	}
	if current.Status == "deleting" || current.Status == "deletion_failed" || current.PublicationName == input.PublicationPath {
		http.Error(w, "deployment is being deleted or destination is unchanged", http.StatusConflict)
		return
	}
	if !validStoredPublicationName(current.PublicationName, id) {
		http.Error(w, "publication binding is invalid", 503)
		return
	}
	if input.ExpectedPublicationPath != a.publicationPaths(id).live() {
		http.Error(w, "publication path changed since review; review the current document root before moving", http.StatusConflict)
		return
	}
	if pending, e := a.movePending(id); e != nil || pending {
		http.Error(w, "a previous move requires reconciliation", 503)
		return
	}
	var publicationIntent int
	if err = a.db.QueryRow(`SELECT count(*) FROM publication_intents WHERE deployment_id=?`, id).Scan(&publicationIntent); err != nil || publicationIntent != 0 {
		http.Error(w, "publication requires reconciliation before moving", 503)
		return
	}
	owner := a.publicationPaths(id)
	destination, err := a.availablePublicationPath(owner, input.PublicationPath)
	if err != nil {
		if errors.Is(err, errPublicationOccupied) {
			http.Error(w, "publication destination is occupied or reserved", 409)
		} else {
			http.Error(w, "destination cannot be inspected", 503)
		}
		return
	}
	source := owner.live()
	old, live, err := owner.checkLive(current.LiveCommit, dirIdentity{uint64(current.LiveDev), uint64(current.LiveIno)})
	if err != nil {
		http.Error(w, "live publication cannot be verified", 503)
		return
	}
	if !live {
		// An unpublished deployment has no namespace entry to move. The same
		// deployment lock keeps waiting workers from observing a partial edit.
		result, e := a.db.Exec(`UPDATE deployments SET publication_name=?,updated_at=? WHERE id=? AND publication_name=? AND status NOT IN ('deleting','deletion_failed')`, input.PublicationPath, a.cfg.Now().UnixNano(), id, current.PublicationName)
		if e != nil {
			http.Error(w, "destination was reserved concurrently", 409)
			return
		}
		if count, _ := result.RowsAffected(); count != 1 {
			http.Error(w, "deployment changed", 409)
			return
		}
		writeJSON(w, map[string]string{"publicationPath": destination})
		return
	}
	// Commit the intent before touching the namespace. UNIQUE destination and
	// the insert trigger guard against concurrent deployment creation.
	inserted, err := a.db.Exec(`INSERT INTO publication_moves(deployment_id,source_name,destination_name,old_dev,old_ino,created_at) SELECT ?,?,?,?,?,? WHERE NOT EXISTS(SELECT 1 FROM deployments WHERE publication_name=?)`, id, current.PublicationName, input.PublicationPath, old.Dev, old.Ino, a.cfg.Now().UnixNano(), input.PublicationPath)
	if err != nil {
		http.Error(w, "publication destination is reserved", 409)
		return
	}
	if count, _ := inserted.RowsAffected(); count != 1 {
		http.Error(w, "publication destination is reserved", 409)
		return
	}
	// SQLite write ownership spans the rename and the durable state update.
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		http.Error(w, "move requires reconciliation", 503)
		return
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE deployments SET updated_at=updated_at WHERE id=? AND publication_name=? AND status NOT IN ('deleting','deletion_failed')`, id, current.PublicationName)
	count := int64(0)
	if err == nil {
		count, err = result.RowsAffected()
	}
	if err != nil || count != 1 {
		http.Error(w, "move requires reconciliation", 503)
		return
	}
	if same, e := sameIdentity(source, old); e != nil || !same {
		http.Error(w, "source identity changed; move blocked", 503)
		return
	}
	if _, e := os.Lstat(destination); !os.IsNotExist(e) {
		http.Error(w, "destination became occupied; move blocked", 503)
		return
	}
	if err = fault("before-move-rename"); err != nil {
		http.Error(w, "move interrupted; reconciliation required", 503)
		return
	}
	if err = unix.Renameat2(unix.AT_FDCWD, source, unix.AT_FDCWD, destination, unix.RENAME_NOREPLACE); err != nil {
		http.Error(w, "atomic move failed; reconciliation required", 503)
		return
	}
	if err = fault("after-move-rename"); err != nil {
		http.Error(w, "move interrupted; reconciliation required", 503)
		return
	}
	if err = fault("before-move-directory-sync"); err != nil {
		http.Error(w, "move directory sync failed; reconciliation required", 503)
		return
	}
	if err = syncDirectory(a.cfg.DeploymentRoot); err != nil {
		http.Error(w, "move directory sync failed; reconciliation required", 503)
		return
	}
	if err = fault("after-move-sync"); err != nil {
		http.Error(w, "move interrupted; reconciliation required", 503)
		return
	}
	verified, verifyErr := sameIdentity(destination, old)
	if verifyErr != nil || !verified {
		http.Error(w, "move outcome ambiguous; reconciliation required", 503)
		return
	}
	if _, verifyErr = os.Lstat(source); !os.IsNotExist(verifyErr) {
		http.Error(w, "move outcome ambiguous; reconciliation required", 503)
		return
	}
	if err = fault("after-move-verify"); err != nil {
		http.Error(w, "move verification interrupted; reconciliation required", 503)
		return
	}
	if _, err = tx.ExecContext(ctx, `UPDATE deployments SET publication_name=?,updated_at=? WHERE id=? AND publication_name=?`, input.PublicationPath, a.cfg.Now().UnixNano(), id, current.PublicationName); err != nil {
		http.Error(w, "move state unavailable; reconciliation required", 503)
		return
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM publication_moves WHERE deployment_id=?`, id); err != nil {
		http.Error(w, "move state unavailable; reconciliation required", 503)
		return
	}
	if err = fault("before-move-state"); err != nil {
		http.Error(w, "move interrupted; reconciliation required", 503)
		return
	}
	if err = tx.Commit(); err != nil {
		http.Error(w, "move state unavailable; reconciliation required", 503)
		return
	}
	writeJSON(w, map[string]string{"publicationPath": destination})
}
