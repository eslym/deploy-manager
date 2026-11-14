package lib

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"golang.org/x/sys/unix"
)

var commitIDPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

func validCommitTarget(id string) bool { return commitIDPattern.MatchString(id) }

type deployment struct {
	ID          string    `json:"id"`
	ProjectID   string    `json:"projectId"`
	Name        string    `json:"name"`
	Commit      string    `json:"commit"`
	TargetKind  string    `json:"targetKind"`
	TargetName  string    `json:"targetName,omitempty"`
	LiveCommit  string    `json:"liveCommit,omitempty"`
	LiveDev     uint64    `json:"-"`
	LiveIno     uint64    `json:"-"`
	Status      string    `json:"status"`
	Error       string    `json:"error,omitempty"`
	Publication string    `json:"publicationPath"`
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

type deploymentJob struct {
	ID           string    `json:"id"`
	DeploymentID string    `json:"deploymentId"`
	Commit       string    `json:"commit"`
	Status       string    `json:"status"`
	Log          string    `json:"log"`
	CreatedAt    time.Time `json:"createdAt"`
	UpdatedAt    time.Time `json:"updatedAt"`
}

type dirIdentity struct{ Dev, Ino uint64 }
type exportLimits struct {
	files   int
	bytes   int64
	timeout time.Duration
}

type publicationFault func(string) error

var publicationFaultPoint publicationFault
var publicationFaultMu sync.RWMutex

func setPublicationFaultPoint(next publicationFault) publicationFault {
	publicationFaultMu.Lock()
	previous := publicationFaultPoint
	publicationFaultPoint = next
	publicationFaultMu.Unlock()
	return previous
}

func fault(point string) error {
	publicationFaultMu.RLock()
	current := publicationFaultPoint
	publicationFaultMu.RUnlock()
	if current != nil {
		return current(point)
	}
	return nil
}

type activeDeploymentJob struct {
	deploymentID string
	cancel       context.CancelFunc
}

func (a *App) registerDeploymentJob(jobID, deploymentID string, cancel context.CancelFunc) func() {
	a.deploymentJobsMu.Lock()
	if a.deploymentJobCancels == nil {
		a.deploymentJobCancels = make(map[string]activeDeploymentJob)
	}
	a.deploymentJobCancels[jobID] = activeDeploymentJob{deploymentID: deploymentID, cancel: cancel}
	a.deploymentJobsMu.Unlock()
	return func() {
		a.deploymentJobsMu.Lock()
		delete(a.deploymentJobCancels, jobID)
		a.deploymentJobsMu.Unlock()
	}
}

func (a *App) cancelDeploymentJobs(deploymentID string) {
	a.deploymentJobsMu.Lock()
	var cancel []context.CancelFunc
	for _, job := range a.deploymentJobCancels {
		if job.deploymentID == deploymentID {
			cancel = append(cancel, job.cancel)
		}
	}
	a.deploymentJobsMu.Unlock()
	for _, stop := range cancel {
		stop()
	}
}

func (a *App) initDeployments() error {
	d := a.cfg
	if d.DeploymentRoot == "" {
		d.DeploymentRoot = d.DatabasePath + ".publications"
	}
	if d.DeployMaxFiles == 0 {
		d.DeployMaxFiles = 10000
	}
	if d.DeployMaxBytes == 0 {
		d.DeployMaxBytes = 1 << 30
	}
	if d.DeployTimeout == 0 {
		d.DeployTimeout = 2 * time.Minute
	}
	if d.DeployConcurrency == 0 {
		d.DeployConcurrency = 2
	}
	if d.DeployGracePeriod == 0 {
		d.DeployGracePeriod = 60 * time.Second
	}
	if d.JobRetention == 0 {
		d.JobRetention = 30 * 24 * time.Hour
	}
	if d.DeployMaxFiles < 1 || d.DeployMaxFiles > 1_000_000 || d.DeployMaxBytes < 1 || d.DeployMaxBytes > 1<<40 || d.DeployTimeout < time.Second || d.DeployTimeout > 10*time.Minute || d.DeployConcurrency < 1 || d.DeployConcurrency > 32 || d.DeployGracePeriod < 0 || d.DeployGracePeriod > 24*time.Hour || d.JobRetention < time.Hour || d.JobRetention > 365*24*time.Hour {
		return errors.New("deployment limits, timeout, concurrency, or retention are outside supported bounds")
	}
	a.cfg = d
	root := filepath.Clean(d.DeploymentRoot)
	absoluteRoot, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	root = absoluteRoot
	if err := ensureOwnedDirectory(root); err != nil {
		return fmt.Errorf("publication root: %w", err)
	}
	a.cfg.DeploymentRoot = root
	rootID, err := identity(root)
	if err != nil {
		return err
	}
	a.publicationRootID = rootID
	for _, q := range []string{
		`CREATE TABLE IF NOT EXISTS deployments(id TEXT PRIMARY KEY,project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE RESTRICT,name TEXT NOT NULL,commit_id TEXT NOT NULL,generation INTEGER NOT NULL DEFAULT 1,live_commit TEXT NOT NULL DEFAULT '',live_dev INTEGER NOT NULL DEFAULT 0,live_ino INTEGER NOT NULL DEFAULT 0,status TEXT NOT NULL DEFAULT 'queued',error TEXT NOT NULL DEFAULT '',created_at INTEGER NOT NULL,updated_at INTEGER NOT NULL,UNIQUE(project_id,name))`,
		`CREATE TABLE IF NOT EXISTS deployment_jobs(id TEXT PRIMARY KEY,deployment_id TEXT NOT NULL REFERENCES deployments(id) ON DELETE CASCADE,commit_id TEXT NOT NULL,generation INTEGER NOT NULL,status TEXT NOT NULL,log TEXT NOT NULL DEFAULT '',created_at INTEGER NOT NULL,updated_at INTEGER NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS publication_intents(deployment_id TEXT PRIMARY KEY REFERENCES deployments(id) ON DELETE CASCADE,job_id TEXT NOT NULL,stage_name TEXT NOT NULL,had_old INTEGER NOT NULL,old_dev INTEGER NOT NULL,old_ino INTEGER NOT NULL,new_dev INTEGER NOT NULL,new_ino INTEGER NOT NULL,phase TEXT NOT NULL,created_at INTEGER NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS publication_cleanup(deployment_id TEXT NOT NULL,job_id TEXT NOT NULL,path_name TEXT NOT NULL,dev INTEGER NOT NULL,ino INTEGER NOT NULL,not_before INTEGER NOT NULL,last_error TEXT NOT NULL DEFAULT '',attempts INTEGER NOT NULL DEFAULT 0,PRIMARY KEY(deployment_id,job_id))`,
		`CREATE TABLE IF NOT EXISTS deployment_removal_paths(deployment_id TEXT NOT NULL,path_name TEXT NOT NULL,dev INTEGER NOT NULL,ino INTEGER NOT NULL,last_error TEXT NOT NULL DEFAULT '',attempts INTEGER NOT NULL DEFAULT 0,PRIMARY KEY(deployment_id,path_name))`,
		`CREATE TABLE IF NOT EXISTS deployment_deletions(deployment_id TEXT PRIMARY KEY,status TEXT NOT NULL,last_error TEXT NOT NULL DEFAULT '',created_at INTEGER NOT NULL,updated_at INTEGER NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS archived_deployment_jobs(id TEXT PRIMARY KEY,deployment_id TEXT NOT NULL,commit_id TEXT NOT NULL,generation INTEGER NOT NULL,status TEXT NOT NULL,log TEXT NOT NULL,created_at INTEGER NOT NULL,updated_at INTEGER NOT NULL)`,
	} {
		if _, err := a.db.Exec(q); err != nil {
			return err
		}
	}
	for _, column := range []struct{ name, definition string }{{"target_kind", `TEXT NOT NULL DEFAULT 'commit'`}, {"target_name", `TEXT NOT NULL DEFAULT ''`}} {
		if err := ensureColumn(a.db, "deployments", column.name, column.definition); err != nil {
			return err
		}
	}
	if err := ensureColumn(a.db, "deployment_deletions", "retry_after", `INTEGER NOT NULL DEFAULT 0`); err != nil {
		return err
	}
	if err := ensureColumn(a.db, "publication_cleanup", "attempts", `INTEGER NOT NULL DEFAULT 0`); err != nil {
		return err
	}
	for _, column := range []struct{ name, definition string }{{"live_dev", `INTEGER NOT NULL DEFAULT 0`}, {"live_ino", `INTEGER NOT NULL DEFAULT 0`}} {
		if err := ensureColumn(a.db, "deployments", column.name, column.definition); err != nil {
			return err
		}
	}
	if err := probeDirectoryExchange(root); err != nil {
		return fmt.Errorf("publication filesystem does not support safe atomic directory exchange: %w", err)
	}
	// Jobs interrupted by process termination are terminalized separately from
	// filesystem intent reconciliation, which uses observed inode identities.
	_, err = a.db.Exec(`UPDATE deployment_jobs SET status='failed',log=CASE WHEN log='' THEN 'interrupted by service restart' ELSE substr(log || char(10) || 'interrupted by service restart',1,8192) END,updated_at=? WHERE status IN ('queued','running')`, d.Now().UnixNano())
	if err != nil {
		return err
	}
	if err := a.reconcilePublications(); err != nil {
		return err
	}
	if err := a.removeOrphanStages(); err != nil {
		return err
	}
	if err := a.validateLivePublicationIdentities(); err != nil {
		return err
	}
	if err := a.resumeDeploymentDeletions(); err != nil {
		log.Printf("deployment deletion recovery: %v", err)
	}
	return nil
}

func (a *App) assertPublicationRoot() error {
	id, err := identity(a.cfg.DeploymentRoot)
	if err != nil {
		return err
	}
	if id != a.publicationRootID {
		return errors.New("publication root identity changed; publication is blocked")
	}
	return nil
}

// lockDeployment serializes publication across workers and service processes.
// The lock file is manager-owned metadata in the already-verified publication root.
func (a *App) lockDeployment(id string) (func(), error) {
	if !validProjectID(id) {
		return nil, errors.New("invalid deployment identifier")
	}
	if err := a.assertPublicationRoot(); err != nil {
		return nil, err
	}
	value, _ := a.deployLocks.LoadOrStore(id, &sync.Mutex{})
	mutex := value.(*sync.Mutex)
	mutex.Lock()
	path := filepath.Join(a.cfg.DeploymentRoot, ".deployment-lock-"+id)
	fd, err := unix.Open(path, unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0600)
	if err != nil {
		mutex.Unlock()
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	if err = unix.Flock(fd, unix.LOCK_EX); err != nil {
		_ = file.Close()
		mutex.Unlock()
		return nil, err
	}
	return func() {
		_ = unix.Flock(fd, unix.LOCK_UN)
		_ = file.Close()
		mutex.Unlock()
	}, nil
}

func (a *App) validateLivePublicationIdentities() error {
	rows, err := a.db.Query(`SELECT id,live_commit,live_dev,live_ino,status FROM deployments ORDER BY id`)
	if err != nil {
		return err
	}
	type liveRecord struct {
		id, commit, status string
		identity           dirIdentity
	}
	var records []liveRecord
	for rows.Next() {
		var r liveRecord
		if err = rows.Scan(&r.id, &r.commit, &r.identity.Dev, &r.identity.Ino, &r.status); err != nil {
			rows.Close()
			return err
		}
		records = append(records, r)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, r := range records {
		if !validProjectID(r.id) {
			return errors.New("deployment record contains an invalid identifier")
		}
		if r.status == "deleting" || r.status == "deletion_failed" {
			continue
		}
		path := a.deploymentPath(r.id)
		if r.commit == "" {
			if _, err := os.Lstat(path); os.IsNotExist(err) {
				continue
			} else if err != nil {
				return err
			}
			return errors.New("publication exists without a recorded live commit; refusing to guess its identity")
		}
		actual, err := identity(path)
		if err != nil {
			return fmt.Errorf("live publication identity cannot be verified: %w", err)
		}
		if actual != r.identity {
			return errors.New("live publication identity changed; refusing to publish or clean up")
		}
	}
	return nil
}

func (a *App) removeOrphanStages() error {
	if err := a.assertPublicationRoot(); err != nil {
		return err
	}
	entries, err := os.ReadDir(a.cfg.DeploymentRoot)
	if err != nil {
		return err
	}
	pattern := regexp.MustCompile(`^\.[0-9a-f]{32}\.stage-[0-9a-f]{32}$`)
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, ".") || !strings.Contains(name, ".stage-") {
			continue
		}
		if !pattern.MatchString(name) {
			return errors.New("unrecognized publication staging path; refusing startup cleanup")
		}
		var present int
		if err := a.db.QueryRow(`SELECT (SELECT count(*) FROM publication_intents WHERE stage_name=?)+(SELECT count(*) FROM publication_cleanup WHERE path_name=?)+(SELECT count(*) FROM deployment_removal_paths WHERE path_name=?)`, name, name, name).Scan(&present); err != nil {
			return err
		}
		if present > 0 {
			continue
		}
		path := filepath.Join(a.cfg.DeploymentRoot, name)
		if _, err := identity(path); err != nil {
			return fmt.Errorf("inspect orphan publication stage: %w", err)
		}
		if err := os.RemoveAll(path); err != nil {
			return fmt.Errorf("remove unpublished stage: %w", err)
		}
		if err := syncDirectory(a.cfg.DeploymentRoot); err != nil {
			return err
		}
	}
	return nil
}

func ensureOwnedDirectory(path string) error {
	if err := os.MkdirAll(path, 0700); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("must be a real directory")
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return err
	}
	if resolved != path {
		return errors.New("publication root must not traverse symbolic links")
	}
	if err := os.Chmod(path, 0755); err != nil {
		return err
	}
	return nil
}

func identity(path string) (dirIdentity, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return dirIdentity{}, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return dirIdentity{}, errors.New("publication path is not a real directory")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return dirIdentity{}, errors.New("filesystem does not expose stable directory identity")
	}
	return dirIdentity{Dev: uint64(stat.Dev), Ino: stat.Ino}, nil
}

func sameIdentity(path string, want dirIdentity) (bool, error) {
	got, err := identity(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	return got == want, nil
}

func probeDirectoryExchange(root string) error {
	a, err := os.MkdirTemp(root, ".probe-a-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(a)
	b, err := os.MkdirTemp(root, ".probe-b-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(b)
	if err = os.WriteFile(filepath.Join(a, "a"), []byte("a"), 0600); err != nil {
		return err
	}
	if err = os.WriteFile(filepath.Join(b, "b"), []byte("b"), 0600); err != nil {
		return err
	}
	ai, err := identity(a)
	if err != nil {
		return err
	}
	bi, err := identity(b)
	if err != nil {
		return err
	}
	if err = unix.Renameat2(unix.AT_FDCWD, a, unix.AT_FDCWD, b, unix.RENAME_EXCHANGE); err != nil {
		return err
	}
	na, err := identity(a)
	if err != nil {
		return err
	}
	nb, err := identity(b)
	if err != nil {
		return err
	}
	if na != bi || nb != ai {
		return errors.New("atomic exchange did not exchange directory identities")
	}
	return nil
}

func syncDirectory(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	e := f.Sync()
	c := f.Close()
	if e != nil {
		return e
	}
	return c
}

func syncTree(root string) error {
	var dirs []string
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, e error) error {
		if e != nil {
			return e
		}
		info, e := os.Lstat(p)
		if e != nil {
			return e
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return errors.New("staged export contains a symbolic link")
		}
		if d.IsDir() {
			dirs = append(dirs, p)
			return nil
		}
		if !info.Mode().IsRegular() {
			return errors.New("staged export contains a non-regular file")
		}
		f, e := os.OpenFile(p, os.O_RDONLY, 0)
		if e != nil {
			return e
		}
		se := f.Sync()
		ce := f.Close()
		if se != nil {
			return se
		}
		return ce
	})
	if err != nil {
		return err
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		if err := syncDirectory(dirs[i]); err != nil {
			return err
		}
	}
	return nil
}

func (a *App) deploymentPath(id string) string { return filepath.Join(a.cfg.DeploymentRoot, id) }
func (a *App) stagePath(id, job string) string {
	return filepath.Join(a.cfg.DeploymentRoot, "."+id+".stage-"+job)
}

func (a *App) enqueueDeployment(deploymentID, commit string, generation int64) (string, error) {
	select {
	case a.deploySlots <- struct{}{}:
	default:
		return "", errors.New("publication queue is full")
	}
	reserved := true
	defer func() {
		if reserved {
			<-a.deploySlots
		}
	}()
	jobID, err := randomID()
	if err != nil {
		return "", err
	}
	now := a.cfg.Now().UTC().UnixNano()
	tx, err := a.db.Begin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	var currentStatus string
	if err = tx.QueryRow(`SELECT status FROM deployments WHERE id=? AND generation=?`, deploymentID, generation).Scan(&currentStatus); err != nil {
		return "", err
	}
	if currentStatus == "deleting" || currentStatus == "deletion_failed" {
		return "", errors.New("deployment is being deleted")
	}
	if _, err = tx.Exec(`INSERT INTO deployment_jobs(id,deployment_id,commit_id,generation,status,log,created_at,updated_at) VALUES(?,?,?,?,'queued','Publication queued',?,?)`, jobID, deploymentID, commit, generation, now, now); err != nil {
		return "", err
	}
	res, err := tx.Exec(`UPDATE deployments SET status='queued',error='',updated_at=? WHERE id=? AND generation=? AND status NOT IN ('deleting','deletion_failed')`, now, deploymentID, generation)
	if err != nil {
		return "", err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return "", errors.New("deployment is being deleted")
	}
	if err = tx.Commit(); err != nil {
		return "", err
	}
	a.deployQueue <- jobID
	reserved = false
	return jobID, nil
}

func randomID() (string, error) {
	b := make([]byte, 16)
	if _, e := rand.Read(b); e != nil {
		return "", e
	}
	return hex.EncodeToString(b), nil
}

func (a *App) startDeploymentWorkers() {
	a.deployQueue = make(chan string, 128)
	a.deployStop = make(chan struct{})
	a.deploySem = make(chan struct{}, a.cfg.DeployConcurrency)
	a.deploySlots = make(chan struct{}, cap(a.deployQueue))
	for i := 0; i < a.cfg.DeployConcurrency; i++ {
		a.deployWG.Add(1)
		go func() {
			defer a.deployWG.Done()
			for {
				select {
				case <-a.deployStop:
					return
				case id := <-a.deployQueue:
					<-a.deploySlots
					a.runDeploymentJob(id)
				}
			}
		}()
	}
}

func (a *App) runDeploymentJob(jobID string) {
	select {
	case a.deploySem <- struct{}{}:
	case <-a.deployStop:
		return
	}
	defer func() { <-a.deploySem }()
	ctx, cancel := context.WithTimeout(context.Background(), a.cfg.DeployTimeout)
	defer cancel()
	var d deployment
	var generation int64
	var projectID string
	var createdAt, updatedAt int64
	err := a.db.QueryRow(`SELECT d.id,d.project_id,d.name,d.commit_id,d.target_kind,d.target_name,d.generation,d.live_commit,d.live_dev,d.live_ino,d.status,d.error,d.created_at,d.updated_at FROM deployment_jobs j JOIN deployments d ON d.id=j.deployment_id WHERE j.id=? AND j.status='queued' AND j.generation=d.generation`, jobID).Scan(&d.ID, &projectID, &d.Name, &d.Commit, &d.TargetKind, &d.TargetName, &generation, &d.LiveCommit, &d.LiveDev, &d.LiveIno, &d.Status, &d.Error, &createdAt, &updatedAt)
	if err != nil {
		_, _ = a.db.Exec(`UPDATE deployment_jobs SET status='superseded',log='deployment was removed or its target changed',updated_at=? WHERE id=? AND status='queued'`, a.cfg.Now().UnixNano(), jobID)
		return
	}
	d.ProjectID = projectID
	d.CreatedAt = time.Unix(0, createdAt).UTC()
	d.UpdatedAt = time.Unix(0, updatedAt).UTC()
	d.Publication = a.deploymentPath(d.ID)
	unregister := a.registerDeploymentJob(jobID, d.ID, cancel)
	defer unregister()
	unlock, err := a.lockDeployment(d.ID)
	if err != nil {
		a.failJob(jobID, d.ID, generation, "publication locking is unavailable")
		return
	}
	defer unlock()
	var liveTarget string
	var currentGeneration int64
	var currentStatus string
	if checkErr := a.db.QueryRow(`SELECT commit_id,generation,status FROM deployments WHERE id=?`, d.ID).Scan(&liveTarget, &currentGeneration, &currentStatus); checkErr != nil || currentGeneration != generation || currentStatus == "deleting" || currentStatus == "deletion_failed" || (d.TargetKind != "branch" && liveTarget != d.Commit) {
		_, _ = a.db.Exec(`UPDATE deployment_jobs SET status='superseded',log=substr(log || char(10) || 'Target changed before work began',1,8192),updated_at=? WHERE id=? AND status='queued'`, a.cfg.Now().UnixNano(), jobID)
		return
	}
	if d.TargetKind == "branch" {
		if err := a.fetchProjectForBranch(ctx, d.ProjectID); err != nil {
			message := "Could not fetch the configured branch; the current publication was kept"
			if errors.Is(err, errBranchFetchRetriesExhausted) {
				message = fmt.Sprintf("Temporary repository transport failures persisted through %d bounded attempts; the current publication was kept", branchFetchMaxAttempts)
			}
			a.failJob(jobID, d.ID, generation, message)
			return
		}
		if err := a.db.QueryRow(`SELECT commit_id FROM project_refs WHERE project_id=? AND kind='branch' AND name=?`, d.ProjectID, d.TargetName).Scan(&d.Commit); err != nil {
			a.failJob(jobID, d.ID, generation, "The configured branch is missing from the cached repository; the current publication was kept")
			return
		}
		if _, err := a.db.Exec(`UPDATE deployment_jobs SET commit_id=?,updated_at=? WHERE id=? AND status='queued'`, d.Commit, a.cfg.Now().UnixNano(), jobID); err != nil {
			a.failJob(jobID, d.ID, generation, "The resolved branch commit could not be recorded; the current publication was kept")
			return
		}
	}
	if checkErr := a.db.QueryRow(`SELECT commit_id,generation,status FROM deployments WHERE id=?`, d.ID).Scan(&liveTarget, &currentGeneration, &currentStatus); checkErr != nil || currentGeneration != generation || currentStatus == "deleting" || currentStatus == "deletion_failed" || (d.TargetKind != "branch" && liveTarget != d.Commit) {
		_, _ = a.db.Exec(`UPDATE deployment_jobs SET status='superseded',log=substr(log || char(10) || 'Target changed before publication',1,8192),updated_at=? WHERE id=? AND status='queued'`, a.cfg.Now().UnixNano(), jobID)
		return
	}
	if d.LiveCommit == d.Commit {
		if liveIdentity, identityErr := identity(a.deploymentPath(d.ID)); identityErr == nil && liveIdentity == (dirIdentity{Dev: d.LiveDev, Ino: d.LiveIno}) {
			a.finishJob(jobID, d.ID, generation, "noop", "Already live at this commit", d.Commit)
			return
		} else {
			a.failJob(jobID, d.ID, generation, "The live publication path is not a safe directory")
			return
		}
	}
	result, err := a.db.Exec(`UPDATE deployment_jobs SET status='running',updated_at=? WHERE id=? AND status='queued'`, a.cfg.Now().UnixNano(), jobID)
	if err != nil {
		return
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return
	}
	if _, err = a.db.Exec(`UPDATE deployments SET status='publishing',error='' WHERE id=? AND generation=?`, d.ID, generation); err != nil {
		return
	}
	select {
	case <-ctx.Done():
		a.failJob(jobID, d.ID, generation, "publication timed out")
		return
	default:
	}
	err = a.publish(ctx, jobID, d, generation)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "target changed") {
			if cleanupErr := a.cleanupUnpublishedStage(d.ID, jobID); cleanupErr != nil {
				log.Printf("cannot clean superseded publication stage for deployment %s: %v", d.ID, cleanupErr)
			}
			_, _ = a.db.Exec(`UPDATE deployment_jobs SET status='superseded',log=substr(log || char(10) || 'Target changed before publication',1,8192),updated_at=? WHERE id=? AND status='running'`, a.cfg.Now().UnixNano(), jobID)
			return
		}
		if cleanupErr := a.cleanupUnpublishedStage(d.ID, jobID); cleanupErr != nil {
			err = errors.New("publication staging cleanup failed; restart reconciliation is required")
		}
		a.failJob(jobID, d.ID, generation, sanitizePublicationError(err))
		return
	}
}

func (a *App) cleanupUnpublishedStage(id, jobID string) error {
	var count int
	if err := a.db.QueryRow(`SELECT count(*) FROM publication_intents WHERE deployment_id=? AND job_id=?`, id, jobID).Scan(&count); err != nil {
		return err
	}
	if count != 0 {
		return nil
	}
	path := a.stagePath(id, jobID)
	if _, err := os.Lstat(path); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	if _, err := identity(path); err != nil {
		return err
	}
	if err := os.RemoveAll(path); err != nil {
		return err
	}
	return syncDirectory(a.cfg.DeploymentRoot)
}

func (a *App) failJob(jobID, id string, generation int64, message string) {
	var hasIntent int
	if err := a.db.QueryRow(`SELECT count(*) FROM publication_intents WHERE deployment_id=?`, id).Scan(&hasIntent); err != nil {
		log.Printf("cannot inspect publication recovery state for deployment %s: %v", id, err)
		message = "Publication outcome could not be recorded. New publications are paused until the service reconciles it."
		hasIntent = 1
	}
	if hasIntent > 0 {
		message = "Publication outcome needs recovery. New publications are paused until the service reconciles it."
	} else {
		message = sanitizePublicationError(errors.New(message))
	}
	message = truncate(message, 512)
	now := a.cfg.Now().UnixNano()
	result, err := a.db.Exec(`UPDATE deployment_jobs SET status='failed',log=substr(log || char(10) || ?,1,8192),updated_at=? WHERE id=? AND status IN ('queued','running')`, message, now, jobID)
	if err != nil {
		log.Printf("cannot record failed deployment job %s: %v", jobID, err)
		return
	}
	if result != nil {
		if affected, rowsErr := result.RowsAffected(); rowsErr != nil {
			log.Printf("cannot inspect failed deployment job update %s: %v", jobID, rowsErr)
		} else if affected == 0 {
			return
		}
	}
	status := "failed"
	if hasIntent > 0 {
		status = "recovery_required"
	}
	if _, err := a.db.Exec(`UPDATE deployments SET status=?,error=?,updated_at=? WHERE id=? AND generation=?`, status, message, now, id, generation); err != nil {
		log.Printf("cannot record failed deployment %s: %v", id, err)
	}
}

func (a *App) finishJob(jobID, id string, generation int64, status, message, commit string) {
	message = truncate(message, 512)
	now := a.cfg.Now().UnixNano()
	tx, err := a.db.Begin()
	if err != nil {
		log.Printf("cannot begin completion of deployment job %s: %v", jobID, err)
		return
	}
	defer tx.Rollback()
	if status == "succeeded" {
		if _, err = tx.Exec(`UPDATE deployments SET live_commit=?,status='live',error='',updated_at=? WHERE id=? AND generation=?`, commit, now, id, generation); err != nil {
			log.Printf("cannot record live deployment %s: %v", id, err)
			return
		}
	} else if _, err = tx.Exec(`UPDATE deployments SET status='live',error='',updated_at=? WHERE id=? AND generation=?`, now, id, generation); err != nil {
		log.Printf("cannot record deployment status %s: %v", id, err)
		return
	}
	if _, err = tx.Exec(`UPDATE deployment_jobs SET status=?,log=substr(log || char(10) || ?,1,8192),updated_at=? WHERE id=?`, status, message, now, jobID); err != nil {
		log.Printf("cannot record completion of deployment job %s: %v", jobID, err)
		return
	}
	if err := tx.Commit(); err != nil {
		log.Printf("cannot commit completion of deployment job %s: %v", jobID, err)
	}
}

func (a *App) publish(ctx context.Context, jobID string, d deployment, generation int64) error {
	if err := a.assertPublicationRoot(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	pm := a.projects
	// Hold a read snapshot only over opening and traversing immutable objects.
	// Cache promotion takes the matching write lock; the scheduler queue itself
	// never holds the project protocol mutex.
	pm.cacheMu.RLock()
	defer pm.cacheMu.RUnlock()
	if err := pm.ensureCacheReady(); err != nil {
		return err
	}
	repo, err := git.PlainOpen(pm.repoPath(d.ProjectID))
	if err != nil {
		return errors.New("project repository is unavailable")
	}
	commit, err := repo.CommitObject(plumbing.NewHash(d.Commit))
	if err != nil || commit.Hash.String() != d.Commit {
		return errors.New("target is not a complete commit in the project repository")
	}
	if size, e := repo.Storer.EncodedObjectSize(commit.Hash); e != nil || size > maxGitCommitObjectBytes {
		return errors.New("target commit is missing or too large")
	}
	stage := a.stagePath(d.ID, jobID)
	if _, err = os.Lstat(stage); err == nil {
		return errors.New("an unresolved publication stage exists")
	} else if !os.IsNotExist(err) {
		return err
	}
	if err = os.Mkdir(stage, 0700); err != nil {
		return err
	}
	hydrator, hydrateErr := a.projectLFSHydrator(d.ProjectID)
	if hydrateErr != nil {
		return errors.New("administrator-approved LFS settings are invalid")
	}
	if err = exportTrackedTree(ctx, repo, commit, stage, exportLimits{files: a.cfg.DeployMaxFiles, bytes: a.cfg.DeployMaxBytes, timeout: a.cfg.DeployTimeout}, hydrator); err != nil {
		return err
	}
	if err = os.Chmod(stage, 0755); err != nil {
		return err
	}
	if err = syncTree(stage); err != nil {
		return fmt.Errorf("sync staged content: %w", err)
	}
	if err = syncDirectory(a.cfg.DeploymentRoot); err != nil {
		return err
	}
	if err = fault("after-stage-fsync"); err != nil {
		return err
	}
	newID, err := identity(stage)
	if err != nil {
		return err
	}
	live := a.deploymentPath(d.ID)
	oldID, oldErr := identity(live)
	hadOld := oldErr == nil
	if oldErr != nil && !os.IsNotExist(oldErr) {
		return oldErr
	}
	// Recheck the saved configuration at the final database/filesystem boundary.
	tx, err := a.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var currentCommit string
	var currentGeneration int64
	var currentStatus string
	if err = tx.QueryRow(`SELECT commit_id,generation,status FROM deployments WHERE id=?`, d.ID).Scan(&currentCommit, &currentGeneration, &currentStatus); err != nil {
		return errors.New("deployment was deleted before publication")
	}
	if currentGeneration != generation || currentStatus == "deleting" || currentStatus == "deletion_failed" || (d.TargetKind != "branch" && currentCommit != d.Commit) {
		return errors.New("publication target changed; job superseded")
	}
	if err = fault("before-intent"); err != nil {
		return err
	}
	if _, err = tx.Exec(`INSERT INTO publication_intents(deployment_id,job_id,stage_name,had_old,old_dev,old_ino,new_dev,new_ino,phase,created_at) VALUES(?,?,?,?,?,?,?,?,?,?)`, d.ID, jobID, filepath.Base(stage), boolInt(hadOld), oldID.Dev, oldID.Ino, newID.Dev, newID.Ino, "intent", a.cfg.Now().UnixNano()); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	if err = fault("after-intent"); err != nil {
		return err
	}
	// Hold a SQLite write transaction from the final generation check through
	// the namespace switch and durable state update. Target edits may supersede
	// work during preparation, but cannot slip between this check and exchange.
	boundaryTx, err := a.db.Begin()
	if err != nil {
		return err
	}
	defer boundaryTx.Rollback()
	guard, err := boundaryTx.Exec(`UPDATE deployments SET updated_at=updated_at WHERE id=? AND generation=?`, d.ID, generation)
	if err != nil {
		return err
	}
	if changed, _ := guard.RowsAffected(); changed != 1 {
		return errors.New("publication target changed; job superseded")
	}
	if hadOld {
		err = unix.Renameat2(unix.AT_FDCWD, stage, unix.AT_FDCWD, live, unix.RENAME_EXCHANGE)
	} else {
		err = unix.Renameat2(unix.AT_FDCWD, stage, unix.AT_FDCWD, live, unix.RENAME_NOREPLACE)
	}
	if err != nil {
		return fmt.Errorf("atomic publication switch failed: %w", err)
	}
	if err = fault("after-exchange"); err != nil {
		return err
	}
	if err = syncDirectory(a.cfg.DeploymentRoot); err != nil {
		return err
	}
	if err = fault("after-directory-fsync"); err != nil {
		return err
	}
	observed, err := identity(live)
	if err != nil || observed != newID {
		return errors.New("publication outcome is ambiguous; recovery required")
	}
	if hadOld {
		oldObserved, identityErr := identity(stage)
		if identityErr != nil || oldObserved != oldID {
			return errors.New("publication outcome is ambiguous; the replaced directory identity changed")
		}
	}
	if err = fault("before-state"); err != nil {
		return err
	}
	tx = boundaryTx
	var latest string
	var latestGen int64
	var latestStatus string
	if err = tx.QueryRow(`SELECT commit_id,generation,status FROM deployments WHERE id=?`, d.ID).Scan(&latest, &latestGen, &latestStatus); err != nil || latestGen != generation || latestStatus == "deleting" || latestStatus == "deletion_failed" || (d.TargetKind != "branch" && latest != d.Commit) {
		return errors.New("target changed after directory switch; publication requires reconciliation")
	}
	if _, err = tx.Exec(`UPDATE deployments SET live_commit=?,live_dev=?,live_ino=?,status='live',error='',updated_at=? WHERE id=? AND generation=?`, d.Commit, newID.Dev, newID.Ino, a.cfg.Now().UnixNano(), d.ID, generation); err != nil {
		return err
	}
	if _, err = tx.Exec(`UPDATE deployment_jobs SET status='succeeded',log=substr(log || char(10) || 'Published successfully',1,8192),updated_at=? WHERE id=?`, a.cfg.Now().UnixNano(), jobID); err != nil {
		return err
	}
	if hadOld {
		if _, err = tx.Exec(`INSERT OR REPLACE INTO publication_cleanup(deployment_id,job_id,path_name,dev,ino,not_before,last_error) VALUES(?,?,?,?,?,?, '')`, d.ID, jobID, filepath.Base(stage), oldID.Dev, oldID.Ino, a.cfg.Now().Add(a.cfg.DeployGracePeriod).UnixNano()); err != nil {
			return err
		}
	}
	if _, err = tx.Exec(`DELETE FROM publication_intents WHERE deployment_id=?`, d.ID); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	if err = fault("after-state"); err != nil {
		return err
	}
	if !hadOld {
		if err = os.Remove(stage); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

func sanitizePublicationError(err error) string {
	s := strings.ToLower(err.Error())
	switch {
	case strings.Contains(s, "lfs"):
		return "Git LFS content could not be safely verified or hydrated; the current live files were kept"
	case strings.Contains(s, "symbolic link") || strings.Contains(s, "symlink"):
		return "This commit contains a symbolic link, which cannot be published"
	case strings.Contains(s, "submodule") || strings.Contains(s, "gitlink"):
		return "This commit contains a submodule, which cannot be published"
	case errors.Is(err, context.DeadlineExceeded):
		return "Publication timed out; the previous files were kept"
	case strings.Contains(s, "file-count limit"):
		return "This commit has too many files to publish; no files were changed"
	case strings.Contains(s, "byte limit"):
		return "This commit is larger than the configured publication limit; no files were changed"
	case strings.Contains(s, "target is not a complete commit"):
		return "The selected commit is no longer available in the project repository"
	case strings.Contains(s, "timeout"):
		return "Publication timed out; no files were changed"
	default:
		return "Publication failed; the current live files were kept"
	}
}

func (a *App) reconcilePublications() error {
	return a.reconcilePublicationsFor("")
}

func (a *App) reconcilePublicationsFor(onlyDeployment string) error {
	query := `SELECT deployment_id,job_id,stage_name,had_old,old_dev,old_ino,new_dev,new_ino FROM publication_intents`
	args := []any{}
	if onlyDeployment != "" {
		query += ` WHERE deployment_id=?`
		args = append(args, onlyDeployment)
	}
	rows, err := a.db.Query(query, args...)
	if err != nil {
		return err
	}
	type intent struct {
		id, job, stage string
		had            int
		old, new       dirIdentity
	}
	var all []intent
	for rows.Next() {
		var x intent
		if err = rows.Scan(&x.id, &x.job, &x.stage, &x.had, &x.old.Dev, &x.old.Ino, &x.new.Dev, &x.new.Ino); err != nil {
			rows.Close()
			return err
		}
		all = append(all, x)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err = rows.Close(); err != nil {
		return err
	}
	for _, x := range all {
		if !validProjectID(x.id) || !validProjectID(x.job) || x.stage != "."+x.id+".stage-"+x.job {
			return errors.New("publication recovery journal contains an unsafe path or identifier")
		}
		live := a.deploymentPath(x.id)
		stage := filepath.Join(a.cfg.DeploymentRoot, x.stage)
		newLive, le := sameIdentity(live, x.new)
		if le != nil {
			return le
		}
		oldLive := false
		if x.had == 1 {
			oldLive, le = sameIdentity(live, x.old)
			if le != nil {
				return le
			}
		} else {
			_, e := os.Lstat(live)
			oldLive = os.IsNotExist(e)
		}
		if newLive {
			if x.had == 1 {
				ok, e := sameIdentity(stage, x.old)
				if e != nil {
					return e
				}
				if !ok {
					return errors.New("ambiguous publication recovery: old directory identity is not at its recorded staging path")
				}
				_, err = a.db.Exec(`INSERT OR REPLACE INTO publication_cleanup(deployment_id,job_id,path_name,dev,ino,not_before,last_error) VALUES(?,?,?,?,?,?, '')`, x.id, x.job, x.stage, x.old.Dev, x.old.Ino, a.cfg.Now().Add(a.cfg.DeployGracePeriod).UnixNano())
				if err != nil {
					return err
				}
			}
			var commit string
			if err = a.db.QueryRow(`SELECT commit_id FROM deployment_jobs WHERE id=?`, x.job).Scan(&commit); err != nil {
				return err
			}
			tx, e := a.db.Begin()
			if e != nil {
				return e
			}
			if _, e = tx.Exec(`UPDATE deployments SET live_commit=?,live_dev=?,live_ino=?,status=CASE WHEN status IN ('deleting','deletion_failed') THEN status ELSE 'live' END,error=CASE WHEN status IN ('deleting','deletion_failed') THEN error ELSE '' END,updated_at=? WHERE id=?`, commit, x.new.Dev, x.new.Ino, a.cfg.Now().UnixNano(), x.id); e != nil {
				tx.Rollback()
				return e
			}
			if _, e = tx.Exec(`UPDATE deployment_jobs SET log=substr(log || char(10) || 'Filesystem outcome reconciled after restart',1,8192),updated_at=? WHERE id=?`, a.cfg.Now().UnixNano(), x.job); e != nil {
				tx.Rollback()
				return e
			}
			if _, e = tx.Exec(`DELETE FROM publication_intents WHERE deployment_id=?`, x.id); e != nil {
				tx.Rollback()
				return e
			}
			if e = tx.Commit(); e != nil {
				return e
			}
		} else if oldLive {
			ok, e := sameIdentity(stage, x.new)
			if e != nil {
				return e
			}
			if ok {
				if e = os.RemoveAll(stage); e != nil {
					return e
				}
				if e = syncDirectory(a.cfg.DeploymentRoot); e != nil {
					return e
				}
			} else if _, e = os.Lstat(stage); !os.IsNotExist(e) {
				return errors.New("ambiguous publication recovery: staged directory identity changed")
			}
			_, err = a.db.Exec(`UPDATE deployments SET status=CASE WHEN status IN ('deleting','deletion_failed') THEN status ELSE 'failed' END,error=CASE WHEN status IN ('deleting','deletion_failed') THEN error ELSE 'publication did not switch before restart' END,updated_at=? WHERE id=?`, a.cfg.Now().UnixNano(), x.id)
			if err != nil {
				return err
			}
			_, err = a.db.Exec(`DELETE FROM publication_intents WHERE deployment_id=?`, x.id)
			if err != nil {
				return err
			}
		} else {
			return errors.New("ambiguous publication recovery: live directory identity matches neither recorded state")
		}
	}
	if onlyDeployment != "" {
		return nil
	}
	return a.cleanupPublications(true)
}

func (a *App) cleanupPublications(startup bool) error {
	if err := a.assertPublicationRoot(); err != nil {
		return err
	}
	rows, err := a.db.Query(`SELECT deployment_id,job_id,path_name,dev,ino,not_before,attempts FROM publication_cleanup ORDER BY not_before`)
	if err != nil {
		return err
	}
	type cleanup struct {
		id, job, path string
		identity      dirIdentity
		before        int64
		attempts      int
	}
	var all []cleanup
	for rows.Next() {
		var c cleanup
		if err = rows.Scan(&c.id, &c.job, &c.path, &c.identity.Dev, &c.identity.Ino, &c.before, &c.attempts); err != nil {
			rows.Close()
			return err
		}
		all = append(all, c)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, c := range all {
		if !validProjectID(c.id) || !validProjectID(c.job) || c.path != "."+c.id+".stage-"+c.job {
			return errors.New("publication cleanup record contains an unsafe path or identifier")
		}
		if c.before > a.cfg.Now().UnixNano() {
			continue
		}
		if err := fault("before-cleanup"); err != nil {
			delay := time.Second << min(c.attempts, 10)
			if _, recordErr := a.db.Exec(`UPDATE publication_cleanup SET last_error='old publication cleanup failed; retry scheduled',attempts=attempts+1,not_before=? WHERE deployment_id=? AND job_id=?`, a.cfg.Now().Add(delay).UnixNano(), c.id, c.job); recordErr != nil {
				return fmt.Errorf("record publication cleanup failure: %w", recordErr)
			}
			continue
		}
		path := filepath.Join(a.cfg.DeploymentRoot, c.path)
		liveIdentity, liveErr := identity(a.deploymentPath(c.id))
		if liveErr != nil {
			return liveErr
		}
		if liveIdentity == c.identity {
			if _, err := a.db.Exec(`UPDATE publication_cleanup SET last_error='cleanup blocked because the old directory is still live' WHERE deployment_id=? AND job_id=?`, c.id, c.job); err != nil {
				return fmt.Errorf("record blocked publication cleanup: %w", err)
			}
			continue
		}
		ok, e := sameIdentity(path, c.identity)
		if e != nil {
			return e
		}
		if !ok {
			if _, e = os.Lstat(path); os.IsNotExist(e) {
				if _, e = a.db.Exec(`DELETE FROM publication_cleanup WHERE deployment_id=? AND job_id=?`, c.id, c.job); e != nil {
					return fmt.Errorf("reconcile removed publication cleanup: %w", e)
				}
				continue
			}
			if _, e = a.db.Exec(`UPDATE publication_cleanup SET last_error='directory identity changed; cleanup blocked' WHERE deployment_id=? AND job_id=?`, c.id, c.job); e != nil {
				return fmt.Errorf("record ambiguous publication cleanup: %w", e)
			}
			continue
		}
		if err = os.RemoveAll(path); err == nil {
			err = syncDirectory(a.cfg.DeploymentRoot)
		}
		if err == nil {
			if err = fault("after-cleanup"); err != nil {
				return err
			}
		}
		if err == nil {
			_, err = a.db.Exec(`DELETE FROM publication_cleanup WHERE deployment_id=? AND job_id=?`, c.id, c.job)
		} else {
			delay := time.Second << min(c.attempts, 10)
			_, recordErr := a.db.Exec(`UPDATE publication_cleanup SET last_error='old publication cleanup failed; retry scheduled',attempts=attempts+1,not_before=? WHERE deployment_id=? AND job_id=?`, a.cfg.Now().Add(delay).UnixNano(), c.id, c.job)
			if recordErr != nil {
				return fmt.Errorf("record publication cleanup failure: %w", recordErr)
			}
		}
		if err != nil {
			return err
		}
	}
	if _, err := a.db.Exec(`DELETE FROM deployment_jobs WHERE updated_at<? AND status IN ('succeeded','failed','superseded','noop') AND id NOT IN (SELECT job_id FROM publication_cleanup) AND id NOT IN (SELECT job_id FROM publication_intents)`, a.cfg.Now().Add(-a.cfg.JobRetention).UnixNano()); err != nil {
		return fmt.Errorf("prune expired deployment jobs: %w", err)
	}
	if _, err := a.db.Exec(`DELETE FROM archived_deployment_jobs WHERE updated_at<?`, a.cfg.Now().Add(-a.cfg.JobRetention).UnixNano()); err != nil {
		return fmt.Errorf("prune expired archived deployment jobs: %w", err)
	}
	return nil
}

func (a *App) publicationCleanupLoop() {
	defer a.deployWG.Done()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-a.deployStop:
			return
		case <-ticker.C:
			if err := a.cleanupPublications(false); err != nil {
				log.Printf("publication cleanup: %v", err)
			}
			if err := a.resumeDeploymentDeletions(); err != nil {
				log.Printf("deployment deletion retry: %v", err)
			}
			a.retryProjectDeletions()
		}
	}
}

func (a *App) listDeployments(w http.ResponseWriter, r *http.Request) {
	if !a.projectAuthorized(w, r, false) {
		return
	}
	rows, err := a.db.Query(`SELECT d.id,d.project_id,d.name,d.commit_id,d.target_kind,d.target_name,d.live_commit,d.status,COALESCE(NULLIF(d.error,''),(SELECT last_error FROM publication_cleanup c WHERE c.deployment_id=d.id ORDER BY c.not_before LIMIT 1),(SELECT last_error FROM deployment_deletions x WHERE x.deployment_id=d.id),''),d.created_at,d.updated_at FROM deployments d ORDER BY d.created_at,d.id`)
	if err != nil {
		http.Error(w, "deployments are temporarily unavailable", 500)
		return
	}
	defer rows.Close()
	out := []deployment{}
	for rows.Next() {
		var d deployment
		var c, u int64
		if err = rows.Scan(&d.ID, &d.ProjectID, &d.Name, &d.Commit, &d.TargetKind, &d.TargetName, &d.LiveCommit, &d.Status, &d.Error, &c, &u); err != nil {
			http.Error(w, "deployments are temporarily unavailable", 500)
			return
		}
		d.CreatedAt = time.Unix(0, c).UTC()
		d.UpdatedAt = time.Unix(0, u).UTC()
		d.Publication = a.deploymentPath(d.ID)
		out = append(out, d)
	}
	if rows.Err() != nil {
		http.Error(w, "deployments are temporarily unavailable", 500)
		return
	}
	writeJSON(w, out)
}

func (a *App) createDeployment(w http.ResponseWriter, r *http.Request) {
	if !a.projectAuthorized(w, r, true) {
		return
	}
	// Project removal holds this lock through cache deletion and the final
	// database commit, so a deployment cannot be inserted into a disappearing
	// project after its no-deployments check.
	a.projects.mu.Lock()
	defer a.projects.mu.Unlock()
	r.Body = http.MaxBytesReader(w, r.Body, 8192)
	var in struct {
		ProjectID  string `json:"projectId"`
		Name       string `json:"name"`
		Commit     string `json:"commit"`
		TargetKind string `json:"targetKind"`
		TargetName string `json:"targetName"`
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if dec.Decode(&in) != nil || !validProjectID(in.ProjectID) || strings.TrimSpace(in.Name) == "" || len(in.Name) > 100 {
		http.Error(w, "enter a deployment name and project", 400)
		return
	}
	if in.TargetKind == "" {
		in.TargetKind = "commit"
	}
	if in.TargetKind != "branch" && in.TargetKind != "commit" {
		http.Error(w, "choose a branch or commit target", 400)
		return
	}
	if in.TargetKind == "branch" {
		if in.TargetName == "" || len(in.TargetName) > 255 {
			http.Error(w, "choose a cached branch", 400)
			return
		}
		if err := a.db.QueryRow(`SELECT commit_id FROM project_refs WHERE project_id=? AND kind='branch' AND name=?`, in.ProjectID, in.TargetName).Scan(&in.Commit); err != nil {
			http.Error(w, "the selected branch is not in the cached branch list", 400)
			return
		}
	} else if !validCommitTarget(in.Commit) {
		http.Error(w, "enter a full 40-character commit ID", 400)
		return
	}
	pm := a.projects
	pm.cacheMu.RLock()
	repo, err := git.PlainOpen(pm.repoPath(in.ProjectID))
	if err == nil {
		var c *object.Commit
		c, err = repo.CommitObject(plumbing.NewHash(in.Commit))
		if err == nil && c.Hash.String() != in.Commit {
			err = errors.New("target commit does not match")
		}
	}
	pm.cacheMu.RUnlock()
	if err != nil {
		http.Error(w, "the selected commit is not available in this project", 400)
		return
	}
	id, err := randomID()
	if err != nil {
		http.Error(w, "deployment could not be saved", 500)
		return
	}
	select {
	case a.deploySlots <- struct{}{}:
	default:
		http.Error(w, "the publication queue is full; try again shortly", http.StatusServiceUnavailable)
		return
	}
	reserved := true
	defer func() {
		if reserved {
			<-a.deploySlots
		}
	}()
	job, err := randomID()
	if err != nil {
		http.Error(w, "deployment could not be queued", 500)
		return
	}
	now := a.cfg.Now().UTC()
	tx, err := a.db.Begin()
	if err != nil {
		http.Error(w, "deployment could not be saved", 500)
		return
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`INSERT INTO deployments(id,project_id,name,commit_id,target_kind,target_name,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?)`, id, in.ProjectID, strings.TrimSpace(in.Name), in.Commit, in.TargetKind, in.TargetName, now.UnixNano(), now.UnixNano()); err != nil {
		http.Error(w, "a deployment with this name already exists or could not be saved", 409)
		return
	}
	if _, err = tx.Exec(`INSERT INTO deployment_jobs(id,deployment_id,commit_id,generation,status,log,created_at,updated_at) VALUES(?,?,?,1,'queued','Publication queued',?,?)`, job, id, in.Commit, now.UnixNano(), now.UnixNano()); err != nil {
		http.Error(w, "deployment could not be queued", 500)
		return
	}
	if err = tx.Commit(); err != nil {
		http.Error(w, "deployment could not be queued", 500)
		return
	}
	a.deployQueue <- job
	reserved = false
	writeJSONStatus(w, 201, map[string]any{"id": id, "jobId": job, "status": "queued"})
}

func (a *App) deploymentJobs(w http.ResponseWriter, r *http.Request) {
	if !a.projectAuthorized(w, r, false) {
		return
	}
	id := r.PathValue("id")
	rows, err := a.db.Query(`SELECT id,deployment_id,commit_id,status,log,created_at,updated_at FROM deployment_jobs WHERE deployment_id=? ORDER BY created_at DESC LIMIT 100`, id)
	if err != nil {
		http.Error(w, "publication history is unavailable", 500)
		return
	}
	defer rows.Close()
	out := []deploymentJob{}
	for rows.Next() {
		var j deploymentJob
		var c, u int64
		if rows.Scan(&j.ID, &j.DeploymentID, &j.Commit, &j.Status, &j.Log, &c, &u) != nil {
			http.Error(w, "publication history is unavailable", 500)
			return
		}
		j.CreatedAt = time.Unix(0, c).UTC()
		j.UpdatedAt = time.Unix(0, u).UTC()
		out = append(out, j)
	}
	writeJSON(w, out)
}

func (a *App) deployNow(w http.ResponseWriter, r *http.Request) {
	if !a.projectAuthorized(w, r, true) {
		return
	}
	id := r.PathValue("id")
	var commit, status string
	var gen int64
	if err := a.db.QueryRow(`SELECT commit_id,generation,status FROM deployments WHERE id=?`, id).Scan(&commit, &gen, &status); err != nil {
		http.NotFound(w, r)
		return
	}
	if status == "deleting" || status == "deletion_failed" {
		http.Error(w, "deployment deletion is in progress", http.StatusConflict)
		return
	}
	job, err := a.enqueueDeployment(id, commit, gen)
	if err != nil {
		if strings.Contains(err.Error(), "queue is full") {
			http.Error(w, "the publication queue is full; try again shortly", http.StatusServiceUnavailable)
		} else {
			http.Error(w, "publication could not be queued", 500)
		}
		return
	}
	writeJSONStatus(w, 202, map[string]string{"jobId": job, "status": "queued"})
}

func (a *App) updateDeploymentTarget(w http.ResponseWriter, r *http.Request) {
	if !a.projectAuthorized(w, r, true) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	var input struct {
		Commit     string `json:"commit"`
		TargetKind string `json:"targetKind"`
		TargetName string `json:"targetName"`
	}
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if decoder.Decode(&input) != nil {
		http.Error(w, "choose a branch or full commit ID", http.StatusBadRequest)
		return
	}
	id := r.PathValue("id")
	var projectID string
	if err := a.db.QueryRow(`SELECT project_id FROM deployments WHERE id=?`, id).Scan(&projectID); err != nil {
		http.NotFound(w, r)
		return
	}
	if input.TargetKind == "" {
		input.TargetKind = "commit"
	}
	if input.TargetKind == "branch" {
		if input.TargetName == "" || len(input.TargetName) > 255 || a.db.QueryRow(`SELECT commit_id FROM project_refs WHERE project_id=? AND kind='branch' AND name=?`, projectID, input.TargetName).Scan(&input.Commit) != nil {
			http.Error(w, "choose a branch from the cached branch list", http.StatusBadRequest)
			return
		}
	} else if input.TargetKind != "commit" || !validCommitTarget(input.Commit) {
		http.Error(w, "choose a branch or full commit ID", http.StatusBadRequest)
		return
	}
	if err := a.assertPublicationRoot(); err != nil {
		http.Error(w, "publication is temporarily blocked pending recovery", http.StatusServiceUnavailable)
		return
	}
	a.projects.cacheMu.RLock()
	repo, err := git.PlainOpen(a.projects.repoPath(projectID))
	if err == nil {
		var commit *object.Commit
		commit, err = repo.CommitObject(plumbing.NewHash(input.Commit))
		if err == nil && commit.Hash.String() != input.Commit {
			err = errors.New("commit identity mismatch")
		}
	}
	a.projects.cacheMu.RUnlock()
	if err != nil {
		http.Error(w, "the selected commit is not available in this project", http.StatusBadRequest)
		return
	}
	select {
	case a.deploySlots <- struct{}{}:
	default:
		http.Error(w, "the publication queue is full; try again shortly", http.StatusServiceUnavailable)
		return
	}
	reserved := true
	defer func() {
		if reserved {
			<-a.deploySlots
		}
	}()
	jobID, err := randomID()
	if err != nil {
		http.Error(w, "publication could not be queued", 500)
		return
	}
	now := a.cfg.Now().UnixNano()
	tx, err := a.db.Begin()
	if err != nil {
		http.Error(w, "publication could not be queued", 500)
		return
	}
	defer tx.Rollback()
	var currentCommit, currentStatus string
	var generation int64
	var currentKind, currentName string
	if err = tx.QueryRow(`SELECT commit_id,target_kind,target_name,generation,status FROM deployments WHERE id=? AND project_id=?`, id, projectID).Scan(&currentCommit, &currentKind, &currentName, &generation, &currentStatus); err != nil {
		http.NotFound(w, r)
		return
	}
	if currentStatus == "deleting" || currentStatus == "deletion_failed" {
		http.Error(w, "deployment deletion is in progress", http.StatusConflict)
		return
	}
	if currentKind == input.TargetKind && currentName == input.TargetName && currentCommit == input.Commit {
		http.Error(w, "this deployment already has that target", http.StatusConflict)
		return
	}
	res, err := tx.Exec(`UPDATE deployments SET commit_id=?,target_kind=?,target_name=?,generation=generation+1,status='queued',error='',updated_at=? WHERE id=? AND generation=? AND status NOT IN ('deleting','deletion_failed')`, input.Commit, input.TargetKind, input.TargetName, now, id, generation)
	if err != nil {
		http.Error(w, "deployment target could not be saved", 500)
		return
	}
	if affected, _ := res.RowsAffected(); affected != 1 {
		http.Error(w, "deployment deletion is in progress", http.StatusConflict)
		return
	}
	if _, err = tx.Exec(`INSERT INTO deployment_jobs(id,deployment_id,commit_id,generation,status,log,created_at,updated_at) VALUES(?,?,?,?, 'queued','Commit target changed; publication queued',?,?)`, jobID, id, input.Commit, generation+1, now, now); err != nil {
		http.Error(w, "publication could not be queued", 500)
		return
	}
	if err = tx.Commit(); err != nil {
		http.Error(w, "publication could not be queued", 500)
		return
	}
	a.deployQueue <- jobID
	reserved = false
	writeJSONStatus(w, http.StatusAccepted, map[string]string{"jobId": jobID, "status": "queued"})
}
