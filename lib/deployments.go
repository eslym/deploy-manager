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

	"github.com/eslym/deploy-manager/lib/ent"
	deploymentmodel "github.com/eslym/deploy-manager/lib/ent/deployment"
	jobmodel "github.com/eslym/deploy-manager/lib/ent/deploymentjob"
	"github.com/eslym/deploy-manager/lib/ent/deploymentremovalpath"
	"github.com/eslym/deploy-manager/lib/ent/projectref"
	"github.com/eslym/deploy-manager/lib/ent/publicationcleanup"
	"github.com/eslym/deploy-manager/lib/ent/publicationintent"
	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"golang.org/x/sys/unix"
)

var commitIDPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

func validCommitTarget(id string) bool { return commitIDPattern.MatchString(id) }

func (a *App) cachedProjectRef(ctx context.Context, projectID, kind, name string) (string, error) {
	ref, err := a.store.ProjectRef.Query().Where(
		projectref.IDEQ(projectID),
		projectref.KindEQ(kind),
		projectref.NameEQ(name),
	).Only(ctx)
	if err != nil {
		return "", err
	}
	return ref.CommitID, nil
}

type deployment struct {
	Rollback         bool   `json:"-"`
	ID               string `json:"id"`
	ProjectID        string `json:"projectId"`
	Name             string `json:"name"`
	Commit           string `json:"commit"`
	TargetKind       string `json:"targetKind"`
	TargetName       string `json:"targetName,omitempty"`
	AutomationPaused bool   `json:"automationPaused"`
	automatic        bool
	automationEpoch  int64
	LiveCommit       string    `json:"liveCommit,omitempty"`
	LiveDev          uint64    `json:"-"`
	LiveIno          uint64    `json:"-"`
	Status           string    `json:"status"`
	Error            string    `json:"error,omitempty"`
	Publication      string    `json:"publicationPath"`
	CreatedAt        time.Time `json:"createdAt"`
	UpdatedAt        time.Time `json:"updatedAt"`
}

type deploymentJob struct {
	ID           string    `json:"id"`
	DeploymentID string    `json:"deploymentId"`
	Commit       string    `json:"commit"`
	Force        bool      `json:"force"`
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
	generation   int64
	cancel       context.CancelFunc
}

func (a *App) registerDeploymentJob(jobID, deploymentID string, generation int64, cancel context.CancelFunc) func() {
	a.deploymentJobsMu.Lock()
	if a.deploymentJobCancels == nil {
		a.deploymentJobCancels = make(map[string]activeDeploymentJob)
	}
	a.deploymentJobCancels[jobID] = activeDeploymentJob{deploymentID: deploymentID, generation: generation, cancel: cancel}
	a.deploymentJobsMu.Unlock()
	return func() {
		a.deploymentJobsMu.Lock()
		delete(a.deploymentJobCancels, jobID)
		a.deploymentJobsMu.Unlock()
	}
}

func (a *App) cancelDeploymentJobs(deploymentID string) {
	a.cancelDeploymentJobsBefore(deploymentID, 0)
}

// A zero cutoff cancels all work (deletion); rollback cancels only generations
// it superseded, never a later explicit Deploy accepted in the meantime.
func (a *App) cancelDeploymentJobsBefore(deploymentID string, generation int64) {
	a.deploymentJobsMu.Lock()
	var cancel []context.CancelFunc
	for _, job := range a.deploymentJobCancels {
		if job.deploymentID == deploymentID && (generation == 0 || job.generation < generation) {
			cancel = append(cancel, job.cancel)
		}
	}
	a.deploymentJobsMu.Unlock()
	for _, stop := range cancel {
		stop()
	}
}

func (a *App) configureDeployments() error {
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
	return nil
}

func (a *App) initDeployments() error {
	d := a.cfg
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
	if err := probeDirectoryExchange(root); err != nil {
		return fmt.Errorf("publication filesystem does not support safe atomic directory exchange: %w", err)
	}
	return a.recoverDeployments()
}

func (a *App) recoverDeployments() error {
	// Jobs interrupted by process termination are terminalized separately from
	// filesystem intent reconciliation, which uses observed inode identities.
	_, err := a.db.Exec(`UPDATE deployment_jobs SET status='failed',log=CASE WHEN log='' THEN 'interrupted by service restart' ELSE substr(log || char(10) || 'interrupted by service restart',1,8192) END,updated_at=? WHERE status='running' OR (status='queued' AND durable=0)`, a.cfg.Now().UnixNano())
	if err != nil {
		return err
	}
	if err := a.requeueInterruptedWebhookJobs(); err != nil {
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
	deployments, err := a.store.Deployment.Query().Order(ent.Asc(deploymentmodel.FieldID)).All(context.Background())
	if err != nil {
		return err
	}
	type liveRecord struct {
		id, commit, status string
		identity           dirIdentity
	}
	records := make([]liveRecord, 0, len(deployments))
	for _, d := range deployments {
		records = append(records, liveRecord{id: d.ID, commit: d.LiveCommit, status: d.Status, identity: dirIdentity{Dev: uint64(d.LiveDev), Ino: uint64(d.LiveIno)}})
	}
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
		intent, err := a.store.PublicationIntent.Query().Where(publicationintent.StageNameEQ(name)).Exist(context.Background())
		if err != nil {
			return err
		}
		cleanup, err := a.store.PublicationCleanup.Query().Where(publicationcleanup.PathNameEQ(name)).Exist(context.Background())
		if err != nil {
			return err
		}
		removal, err := a.store.DeploymentRemovalPath.Query().Where(deploymentremovalpath.PathNameEQ(name)).Exist(context.Background())
		if err != nil {
			return err
		}
		if intent || cleanup || removal {
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

// A reservation keeps queue admission separate from each operation's durable
// transaction. Defer release on every path; enqueue transfers it to the worker.
type deploymentSlot struct {
	app      *App
	reserved bool
}

func (a *App) reserveDeploymentSlot() (*deploymentSlot, bool) {
	select {
	case a.deploySlots <- struct{}{}:
		return &deploymentSlot{app: a, reserved: true}, true
	default:
		return nil, false
	}
}

func (s *deploymentSlot) release() {
	if s.reserved {
		<-s.app.deploySlots
		s.reserved = false
	}
}

func (s *deploymentSlot) enqueue(jobID string) {
	s.app.deployQueue <- jobID
	s.reserved = false
}

func (a *App) enqueueDeployment(deploymentID, commit string, generation int64) (string, error) {
	slot, ok := a.reserveDeploymentSlot()
	if !ok {
		return "", errors.New("publication queue is full")
	}
	defer slot.release()
	jobID, err := randomID()
	if err != nil {
		return "", err
	}
	now := a.cfg.Now().UTC().UnixNano()
	tx, err := a.store.Tx(context.Background())
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	current, err := tx.Deployment.Query().Where(deploymentmodel.ID(deploymentID), deploymentmodel.Generation(generation)).Only(context.Background())
	if err != nil {
		return "", err
	}
	if current.Status == "deleting" || current.Status == "deletion_failed" {
		return "", errors.New("deployment is being deleted")
	}
	if _, err = tx.DeploymentJob.Create().SetID(jobID).SetDeploymentID(deploymentID).SetCommitID(commit).SetGeneration(generation).SetStatus("queued").SetLog("Publication queued").SetCreatedAt(now).SetUpdatedAt(now).Save(context.Background()); err != nil {
		return "", err
	}
	res, err := tx.Deployment.Update().Where(deploymentmodel.ID(deploymentID), deploymentmodel.Generation(generation), deploymentmodel.StatusNotIn("deleting", "deletion_failed")).SetStatus("queued").SetError("").SetUpdatedAt(now).Save(context.Background())
	if err != nil {
		return "", err
	}
	if res != 1 {
		return "", errors.New("deployment is being deleted")
	}
	if err = tx.Commit(); err != nil {
		return "", err
	}
	slot.enqueue(jobID)
	return jobID, nil
}

func randomID() (string, error) {
	b := make([]byte, 16)
	if _, e := rand.Read(b); e != nil {
		return "", e
	}
	return hex.EncodeToString(b), nil
}

const deploymentQueueCapacity = 160

func restoreQueuedDeploymentJobs(store *ent.Client, capacity int) ([]string, error) {
	ids, err := store.DeploymentJob.Query().Where(jobmodel.StatusEQ("queued"), jobmodel.DurableEQ(1)).Order(ent.Asc(jobmodel.FieldCreatedAt), ent.Asc(jobmodel.FieldID)).Limit(capacity).IDs(context.Background())
	if err != nil {
		return nil, err
	}
	return ids, nil
}

func (a *App) startDeploymentWorkers(restoredJobs []string) {
	a.deployQueue = make(chan string, deploymentQueueCapacity)
	a.deployStop = make(chan struct{})
	a.deploySem = make(chan struct{}, a.cfg.DeployConcurrency)
	a.deploySlots = make(chan struct{}, cap(a.deployQueue))
	for _, id := range restoredJobs {
		a.deploySlots <- struct{}{}
		a.deployQueue <- id
	}
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
	job, err := a.store.DeploymentJob.Query().Where(jobmodel.ID(jobID), jobmodel.Status("queued")).Only(context.Background())
	if err != nil {
		return
	}
	deploymentID, generation := job.DeploymentID, job.Generation
	if err := fault("before-job-lock"); err != nil {
		a.failJob(jobID, deploymentID, generation, "publication locking was interrupted")
		return
	}
	unlock, err := a.lockDeployment(deploymentID)
	if err != nil {
		a.failJob(jobID, deploymentID, generation, "publication locking is unavailable")
		return
	}
	defer unlock()
	// Workers may acquire this lock in a different order than jobs were
	// accepted. Execute any queued predecessors first, under the same lock,
	// rather than allowing a later tip publication/no-op to precede rollback.
	// SQLite's insertion order is the acceptance order, independent of clocks
	// and of the order producers manage to send their committed job IDs.
	rows, err := a.db.Query(`SELECT id FROM deployment_jobs WHERE deployment_id=? AND status='queued' AND rowid<=(SELECT rowid FROM deployment_jobs WHERE id=?) ORDER BY rowid`, deploymentID, jobID)
	if err != nil {
		return
	}
	var jobs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return
		}
		jobs = append(jobs, id)
	}
	err = rows.Err()
	if closeErr := rows.Close(); err != nil || closeErr != nil {
		return
	}
	for _, id := range jobs {
		select {
		case <-a.deployStop:
			return
		default:
		}
		a.runLockedDeploymentJob(id)
	}
}

// The caller owns the deployment lock; each job gets its own timeout and
// cancellation registration, including predecessors picked up by a later worker.
func (a *App) runLockedDeploymentJob(jobID string) {
	ctx, cancel := context.WithTimeout(context.Background(), a.cfg.DeployTimeout)
	defer cancel()
	var d deployment
	var generation int64
	var forceRedeploy bool
	job, err := a.store.DeploymentJob.Query().Where(jobmodel.ID(jobID), jobmodel.Status("queued")).WithDeployment().Only(ctx)
	if err != nil || job.Edges.Deployment == nil || job.Generation != job.Edges.Deployment.Generation {
		_, _ = a.store.DeploymentJob.UpdateOneID(jobID).Where(jobmodel.Status("queued")).SetStatus("superseded").SetLog("deployment was removed or its target changed").SetUpdatedAt(a.cfg.Now().UnixNano()).Save(ctx)
		return
	}
	model := job.Edges.Deployment
	generation = job.Generation
	d.ID, d.ProjectID, d.Name = model.ID, model.ProjectID, model.Name
	d.Commit, d.TargetKind, d.TargetName = model.CommitID, model.TargetKind, model.TargetName
	d.LiveCommit, d.LiveDev, d.LiveIno = model.LiveCommit, uint64(model.LiveDev), uint64(model.LiveIno)
	d.Status, d.Error = model.Status, model.Error
	d.automatic, d.automationEpoch = job.Automatic != 0, job.AutomationEpoch
	forceRedeploy = job.ForceRedeploy != 0
	d.CreatedAt = time.Unix(0, model.CreatedAt).UTC()
	d.UpdatedAt = time.Unix(0, model.UpdatedAt).UTC()
	d.Rollback = job.RollbackCommit != ""
	d.Publication = a.deploymentPath(d.ID)
	unregister := a.registerDeploymentJob(jobID, d.ID, generation, cancel)
	defer unregister()
	if _, claimErr := a.store.DeploymentJob.UpdateOneID(jobID).Where(jobmodel.Status("queued")).SetStatus("running").SetUpdatedAt(a.cfg.Now().UnixNano()).Save(ctx); claimErr != nil {
		return
	}
	var liveTarget string
	var currentGeneration int64
	var currentStatus string
	// Another job in this generation may have published while this worker was
	// waiting for the deployment lock (notably a preceding rollback).
	if checkErr := a.db.QueryRow(`SELECT commit_id,generation,status,live_commit,live_dev,live_ino FROM deployments WHERE id=?`, d.ID).Scan(&liveTarget, &currentGeneration, &currentStatus, &d.LiveCommit, &d.LiveDev, &d.LiveIno); checkErr != nil || currentGeneration != generation || currentStatus == "deleting" || currentStatus == "deletion_failed" || (d.TargetKind != "branch" && liveTarget != d.Commit) {
		_, _ = a.db.Exec(`UPDATE deployment_jobs SET status='superseded',log=substr(log || char(10) || 'Target changed before work began',1,8192),updated_at=? WHERE id=? AND status='running'`, a.cfg.Now().UnixNano(), jobID)
		return
	}
	if d.TargetKind == "branch" || forceRedeploy {
		if err := a.fetchProjectForBranch(ctx, d.ProjectID); err != nil {
			message := "Could not refresh the configured Git target; the current publication was kept"
			if errors.Is(err, errBranchFetchRetriesExhausted) {
				message = fmt.Sprintf("Temporary repository transport failures persisted through %d bounded attempts; the current publication was kept", branchFetchMaxAttempts)
			}
			a.failJob(jobID, d.ID, generation, message)
			return
		}
		if d.Rollback {
			d.Commit = job.RollbackCommit
			a.projects.cacheMu.RLock()
			repo, openErr := git.PlainOpen(a.projects.repoPath(d.ProjectID))
			if openErr == nil {
				openErr = rollbackAncestor(ctx, repo, d.TargetName, d.Commit)
			}
			a.projects.cacheMu.RUnlock()
			if openErr != nil {
				a.failJob(jobID, d.ID, generation, "Rollback commit is unavailable or no longer an ancestor of the refreshed branch; live files were kept")
				return
			}
		} else if d.TargetKind == "branch" || d.TargetKind == "tag" {
			kind := d.TargetKind
			if commit, err := a.cachedProjectRef(ctx, d.ProjectID, kind, d.TargetName); err != nil {
				a.failJob(jobID, d.ID, generation, "The configured Git target is missing from the refreshed repository; the current publication was kept")
				return
			} else {
				d.Commit = commit
			}
			if kind == "tag" {
				// Hard re-deploy is the explicit tag-refresh operation. Persist its
				// new pin after fetch, before export; later failures and restarts keep it.
				result, updateErr := a.db.Exec(`UPDATE deployments SET commit_id=?,updated_at=? WHERE id=? AND generation=? AND target_kind='tag' AND target_name=?`, d.Commit, a.cfg.Now().UnixNano(), d.ID, generation, d.TargetName)
				if updateErr != nil {
					a.failJob(jobID, d.ID, generation, "The refreshed tag resolution could not be saved; the current publication was kept")
					return
				}
				if affected, _ := result.RowsAffected(); affected != 1 {
					_, _ = a.db.Exec(`UPDATE deployment_jobs SET status='superseded',log=substr(log || char(10) || 'Target changed before refreshed tag resolution was saved',1,8192),updated_at=? WHERE id=? AND status='running'`, a.cfg.Now().UnixNano(), jobID)
					return
				}
			}
		}
		if _, err := a.db.Exec(`UPDATE deployment_jobs SET commit_id=?,updated_at=? WHERE id=? AND status='running'`, d.Commit, a.cfg.Now().UnixNano(), jobID); err != nil {
			a.failJob(jobID, d.ID, generation, "The resolved branch commit could not be recorded; the current publication was kept")
			return
		}
	}
	currentDeployment, checkErr := a.store.Deployment.Get(ctx, d.ID)
	if checkErr != nil || currentDeployment.Generation != generation || currentDeployment.Status == "deleting" || currentDeployment.Status == "deletion_failed" || (d.TargetKind != "branch" && currentDeployment.CommitID != d.Commit) {
		_, _ = a.db.Exec(`UPDATE deployment_jobs SET status='superseded',log=substr(log || char(10) || 'Target changed before publication',1,8192),updated_at=? WHERE id=? AND status='running'`, a.cfg.Now().UnixNano(), jobID)
		return
	}
	if d.automatic {
		currentDeployment, err := a.store.Deployment.Get(ctx, d.ID)
		if err != nil {
			a.failJob(jobID, d.ID, generation, "Automatic deployment status could not be checked; the current publication was kept")
			return
		}
		if currentDeployment.AutomationPaused != 0 || currentDeployment.AutomationEpoch != d.automationEpoch {
			a.finishAutomaticFetch(jobID, d.ID, generation, currentDeployment.AutomationPaused != 0)
			return
		}
	}
	if d.LiveCommit == d.Commit && !forceRedeploy && !d.Rollback {
		if liveIdentity, identityErr := identity(a.deploymentPath(d.ID)); identityErr == nil && liveIdentity == (dirIdentity{Dev: d.LiveDev, Ino: d.LiveIno}) {
			a.finishJob(jobID, d.ID, generation, "noop", "Already live at this commit", d.Commit)
			return
		} else {
			a.failJob(jobID, d.ID, generation, "The live publication path is not a safe directory")
			return
		}
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
			// Supersession can arrive after intent is durable but before the
			// generation guard permits the switch. Reconcile that known outcome
			// under the deployment lock before the replacement worker proceeds.
			if recoveryErr := a.reconcilePublicationsFor(d.ID); recoveryErr != nil {
				log.Printf("cannot reconcile superseded publication for deployment %s: %v", d.ID, recoveryErr)
				return
			}
			if cleanupErr := a.cleanupUnpublishedStage(d.ID, jobID); cleanupErr != nil {
				log.Printf("cannot clean superseded publication stage for deployment %s: %v", d.ID, cleanupErr)
			}
			if d.automatic {
				a.finishAutomaticFetch(jobID, d.ID, generation, false)
			} else {
				_, _ = a.db.Exec(`UPDATE deployment_jobs SET status='superseded',log=substr(log || char(10) || 'Target changed before publication',1,8192),updated_at=? WHERE id=? AND status='running'`, a.cfg.Now().UnixNano(), jobID)
			}
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
	pending, err := a.store.PublicationIntent.Query().Where(publicationintent.IDEQ(id), publicationintent.JobIDEQ(jobID)).Exist(context.Background())
	if err != nil {
		return err
	}
	if pending {
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
	ctx := context.Background()
	tx, err := a.store.Tx(ctx)
	if err != nil {
		log.Printf("cannot begin failure record for deployment job %s: %v", jobID, err)
		return
	}
	defer tx.Rollback()
	hasIntent, err := tx.PublicationIntent.Query().Where(publicationintent.IDEQ(id)).Exist(ctx)
	if err != nil {
		log.Printf("cannot inspect publication recovery state for deployment %s: %v", id, err)
		message = "Publication outcome could not be recorded. New publications are paused until the service reconciles it."
		hasIntent = true
	}
	if hasIntent {
		message = "Publication outcome needs recovery. New publications are paused until the service reconciles it."
	} else {
		message = sanitizePublicationError(errors.New(message))
	}
	message = truncate(message, 512)
	now := a.cfg.Now().UnixNano()
	job, err := tx.DeploymentJob.Get(ctx, jobID)
	if err != nil {
		log.Printf("cannot read failed deployment job %s: %v", jobID, err)
		return
	}
	updated, err := tx.DeploymentJob.Update().Where(jobmodel.IDEQ(jobID), jobmodel.StatusIn("queued", "running")).SetStatus("failed").SetLog(appendJobLog(job.Log, message)).SetUpdatedAt(now).Save(ctx)
	if err != nil {
		log.Printf("cannot record failed deployment job %s: %v", jobID, err)
		return
	}
	if updated == 0 {
		return
	}
	status := "failed"
	if hasIntent {
		status = "recovery_required"
	}
	if _, err = tx.Deployment.Update().Where(deploymentmodel.IDEQ(id), deploymentmodel.GenerationEQ(generation)).SetStatus(status).SetError(message).SetUpdatedAt(now).Save(ctx); err != nil {
		log.Printf("cannot record failed deployment %s: %v", id, err)
		return
	}
	if err = tx.Commit(); err != nil {
		log.Printf("cannot commit failed deployment record %s: %v", id, err)
	}
}

func (a *App) finishJob(jobID, id string, generation int64, status, message, commit string) {
	message = truncate(message, 512)
	now := a.cfg.Now().UnixNano()
	tx, err := a.store.Tx(context.Background())
	if err != nil {
		log.Printf("cannot begin completion of deployment job %s: %v", jobID, err)
		return
	}
	defer tx.Rollback()
	update := tx.Deployment.Update().Where(deploymentmodel.ID(id), deploymentmodel.Generation(generation))
	if status == "succeeded" {
		update.SetLiveCommit(commit)
	}
	if _, err = update.SetStatus("live").SetError("").SetUpdatedAt(now).Save(context.Background()); err != nil {
		log.Printf("cannot record live deployment %s: %v", id, err)
		return
	}
	job, err := tx.DeploymentJob.Get(context.Background(), jobID)
	if err != nil {
		log.Printf("cannot read deployment job %s for completion: %v", jobID, err)
		return
	}
	if _, err = tx.DeploymentJob.UpdateOneID(jobID).SetStatus(status).SetLog(truncate(job.Log+"\n"+message, 8192)).SetUpdatedAt(now).Save(context.Background()); err != nil {
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
	cacheLocked := true
	defer func() {
		if cacheLocked {
			pm.cacheMu.RUnlock()
		}
	}()
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
	pm.cacheMu.RUnlock()
	cacheLocked = false
	if err = fault("after-stage-fsync"); err != nil {
		return err
	}
	// A newer successful fetch may have promoted a force-pushed graph while
	// content was prepared. Hold its snapshot through the namespace switch.
	pm.cacheMu.RLock()
	cacheLocked = true
	if err := pm.ensureCacheReady(); err != nil {
		return err
	}
	if d.Rollback {
		latestRepo, openErr := git.PlainOpen(pm.repoPath(d.ProjectID))
		if openErr != nil {
			return errors.New("rollback repository unavailable")
		}
		if err = rollbackAncestor(ctx, latestRepo, d.TargetName, d.Commit); err != nil {
			return err
		}
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
	dbCtx := context.Background()
	// Recheck the saved configuration at the final database/filesystem boundary.
	tx, err := a.store.Tx(dbCtx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	current, err := tx.Deployment.Get(dbCtx, d.ID)
	if err != nil {
		return errors.New("deployment was deleted before publication")
	}
	if current.Generation != generation || current.Status == "deleting" || current.Status == "deletion_failed" || (d.TargetKind != "branch" && current.CommitID != d.Commit) || (d.automatic && (current.AutomationPaused != 0 || current.AutomationEpoch != d.automationEpoch)) {
		return errors.New("publication target changed; job superseded")
	}
	if err = fault("before-intent"); err != nil {
		return err
	}
	if _, err = tx.PublicationIntent.Create().SetID(d.ID).SetJobID(jobID).SetStageName(filepath.Base(stage)).SetHadOld(boolInt(hadOld)).SetOldDev(int64(oldID.Dev)).SetOldIno(int64(oldID.Ino)).SetNewDev(int64(newID.Dev)).SetNewIno(int64(newID.Ino)).SetPhase("intent").SetCreatedAt(a.cfg.Now().UnixNano()).Save(dbCtx); err != nil {
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
	boundaryTx, err := a.store.Tx(dbCtx)
	if err != nil {
		return err
	}
	defer boundaryTx.Rollback()
	boundaryDeployment, err := boundaryTx.Deployment.Get(dbCtx, d.ID)
	if err != nil {
		return errors.New("deployment was deleted before publication")
	}
	guard := boundaryTx.Deployment.Update().Where(
		deploymentmodel.IDEQ(d.ID),
		deploymentmodel.GenerationEQ(generation),
		deploymentmodel.StatusNotIn("deleting", "deletion_failed"),
	)
	if d.automatic {
		guard.Where(deploymentmodel.AutomationPausedEQ(0), deploymentmodel.AutomationEpochEQ(d.automationEpoch))
	}
	changed, err := guard.SetUpdatedAt(boundaryDeployment.UpdatedAt).Save(dbCtx)
	if err != nil {
		return err
	}
	if changed != 1 {
		// No namespace operation has happened. Remove only this job's intent
		// while holding the same write transaction, so cleanup is safe now and
		// a subsequent manual publication does not require restart recovery.
		if _, err = boundaryTx.PublicationIntent.Delete().Where(publicationintent.IDEQ(d.ID), publicationintent.JobIDEQ(jobID)).Exec(dbCtx); err != nil {
			return err
		}
		if err = boundaryTx.Commit(); err != nil {
			return err
		}
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
	latest, err := boundaryTx.Deployment.Get(dbCtx, d.ID)
	if err != nil || latest.Generation != generation || latest.Status == "deleting" || latest.Status == "deletion_failed" || (d.TargetKind != "branch" && latest.CommitID != d.Commit) {
		return errors.New("target changed after directory switch; publication requires reconciliation")
	}
	if _, err = boundaryTx.Deployment.Update().Where(deploymentmodel.IDEQ(d.ID), deploymentmodel.GenerationEQ(generation)).SetLiveCommit(d.Commit).SetLiveDev(int64(newID.Dev)).SetLiveIno(int64(newID.Ino)).SetStatus("live").SetError("").SetUpdatedAt(a.cfg.Now().UnixNano()).Save(dbCtx); err != nil {
		return err
	}
	job, err := boundaryTx.DeploymentJob.Get(dbCtx, jobID)
	if err != nil {
		return err
	}
	if _, err = boundaryTx.DeploymentJob.UpdateOneID(jobID).SetStatus("succeeded").SetLog(appendJobLog(job.Log, "Published successfully")).SetUpdatedAt(a.cfg.Now().UnixNano()).Save(dbCtx); err != nil {
		return err
	}
	if hadOld {
		if _, err = boundaryTx.PublicationCleanup.Delete().Where(publicationcleanup.IDEQ(d.ID), publicationcleanup.JobIDEQ(jobID)).Exec(dbCtx); err != nil {
			return err
		}
		if _, err = boundaryTx.PublicationCleanup.Create().SetID(d.ID).SetJobID(jobID).SetPathName(filepath.Base(stage)).SetDev(int64(oldID.Dev)).SetIno(int64(oldID.Ino)).SetNotBefore(a.cfg.Now().Add(a.cfg.DeployGracePeriod).UnixNano()).SetLastError("").Save(dbCtx); err != nil {
			return err
		}
	}
	if _, err = boundaryTx.PublicationIntent.Delete().Where(publicationintent.IDEQ(d.ID)).Exec(dbCtx); err != nil {
		return err
	}
	if err = boundaryTx.Commit(); err != nil {
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

func appendJobLog(log, message string) string {
	combined := []rune(log + "\n" + message)
	if len(combined) > 8192 {
		combined = combined[:8192]
	}
	return string(combined)
}

func sanitizePublicationError(err error) string {
	s := strings.ToLower(err.Error())
	switch {
	case strings.Contains(s, "rollback"):
		return "Rollback commit is unavailable or no longer an ancestor of the configured branch; the current live files were kept"
	case strings.Contains(s, "private ssh lfs authentication"):
		return "Private repository LFS authentication failed. Verify the repository key and approved SSH host key, and confirm the server provides git-lfs-authenticate. Pure-SSH LFS transfers are not supported. Existing published files were kept."
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
	query := a.store.PublicationIntent.Query()
	if onlyDeployment != "" {
		query = query.Where(publicationintent.IDEQ(onlyDeployment))
	}
	intents, err := query.All(context.Background())
	if err != nil {
		return err
	}
	type intent struct {
		id, job, stage string
		had            int
		old, new       dirIdentity
	}
	all := make([]intent, 0, len(intents))
	for _, row := range intents {
		all = append(all, intent{id: row.ID, job: row.JobID, stage: row.StageName, had: row.HadOld, old: dirIdentity{Dev: uint64(row.OldDev), Ino: uint64(row.OldIno)}, new: dirIdentity{Dev: uint64(row.NewDev), Ino: uint64(row.NewIno)}})
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
			}
			job, getErr := a.store.DeploymentJob.Get(context.Background(), x.job)
			if getErr != nil {
				return getErr
			}
			commit := job.CommitID
			if commit == "" {
				err = errors.New("publication job has no commit")
				return err
			}
			ctx := context.Background()
			tx, e := a.store.Tx(ctx)
			if e != nil {
				return e
			}
			defer tx.Rollback()
			deployment, queryErr := tx.Deployment.Get(ctx, x.id)
			if queryErr != nil {
				return queryErr
			}
			update := tx.Deployment.UpdateOneID(x.id).SetLiveCommit(commit).SetLiveDev(int64(x.new.Dev)).SetLiveIno(int64(x.new.Ino)).SetUpdatedAt(a.cfg.Now().UnixNano())
			if deployment.Status != "deleting" && deployment.Status != "deletion_failed" {
				update.SetStatus("live").SetError("")
			}
			if _, e = update.Save(ctx); e != nil {
				return e
			}
			if _, e = tx.DeploymentJob.UpdateOneID(x.job).SetLog(appendJobLog(job.Log, "Filesystem outcome reconciled after restart")).SetUpdatedAt(a.cfg.Now().UnixNano()).Save(ctx); e != nil {
				return e
			}
			if x.had == 1 {
				if _, e = tx.PublicationCleanup.Delete().Where(publicationcleanup.IDEQ(x.id), publicationcleanup.JobIDEQ(x.job)).Exec(ctx); e != nil {
					return e
				}
				if _, e = tx.PublicationCleanup.Create().SetID(x.id).SetJobID(x.job).SetPathName(x.stage).SetDev(int64(x.old.Dev)).SetIno(int64(x.old.Ino)).SetNotBefore(a.cfg.Now().Add(a.cfg.DeployGracePeriod).UnixNano()).SetLastError("").Save(ctx); e != nil {
					return e
				}
			}
			if _, e = tx.PublicationIntent.Delete().Where(publicationintent.IDEQ(x.id)).Exec(ctx); e != nil {
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
			ctx := context.Background()
			tx, txErr := a.store.Tx(ctx)
			if txErr != nil {
				return txErr
			}
			defer tx.Rollback()
			deployment, queryErr := tx.Deployment.Get(ctx, x.id)
			if queryErr == nil && deployment.Status != "deleting" && deployment.Status != "deletion_failed" {
				if _, txErr = tx.Deployment.UpdateOneID(x.id).SetStatus("failed").SetError("publication did not switch before restart").SetUpdatedAt(a.cfg.Now().UnixNano()).Save(ctx); txErr != nil {
					return txErr
				}
			} else if queryErr != nil && !ent.IsNotFound(queryErr) {
				return queryErr
			}
			if _, txErr = tx.PublicationIntent.Delete().Where(publicationintent.IDEQ(x.id)).Exec(ctx); txErr != nil {
				return txErr
			}
			if txErr = tx.Commit(); txErr != nil {
				return txErr
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
	cleanups, err := a.store.PublicationCleanup.Query().Order(publicationcleanup.ByNotBefore()).All(context.Background())
	if err != nil {
		return err
	}
	type cleanup struct {
		id, job, path string
		identity      dirIdentity
		before        int64
		attempts      int
	}
	all := make([]cleanup, 0, len(cleanups))
	for _, row := range cleanups {
		all = append(all, cleanup{id: row.ID, job: row.JobID, path: row.PathName, identity: dirIdentity{Dev: uint64(row.Dev), Ino: uint64(row.Ino)}, before: row.NotBefore, attempts: row.Attempts})
	}
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
			if _, err := a.store.PublicationCleanup.Update().Where(publicationcleanup.IDEQ(c.id), publicationcleanup.JobIDEQ(c.job)).SetLastError("cleanup blocked because the old directory is still live").Save(context.Background()); err != nil {
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
				if _, e = a.store.PublicationCleanup.Delete().Where(publicationcleanup.IDEQ(c.id), publicationcleanup.JobIDEQ(c.job)).Exec(context.Background()); e != nil {
					return fmt.Errorf("reconcile removed publication cleanup: %w", e)
				}
				continue
			}
			if _, e = a.store.PublicationCleanup.Update().Where(publicationcleanup.IDEQ(c.id), publicationcleanup.JobIDEQ(c.job)).SetLastError("directory identity changed; cleanup blocked").Save(context.Background()); e != nil {
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
			_, err = a.store.PublicationCleanup.Delete().Where(publicationcleanup.IDEQ(c.id), publicationcleanup.JobIDEQ(c.job)).Exec(context.Background())
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
	rows, err := a.db.Query(`SELECT d.id,d.project_id,d.name,d.commit_id,d.target_kind,d.target_name,d.live_commit,d.status,COALESCE(NULLIF(d.error,''),(SELECT last_error FROM publication_cleanup c WHERE c.deployment_id=d.id ORDER BY c.not_before LIMIT 1),(SELECT last_error FROM deployment_deletions x WHERE x.deployment_id=d.id),''),d.created_at,d.updated_at,d.automation_paused FROM deployments d ORDER BY d.created_at,d.id`)
	if err != nil {
		http.Error(w, "deployments are temporarily unavailable", 500)
		return
	}
	defer rows.Close()
	out := []deployment{}
	for rows.Next() {
		var d deployment
		var c, u int64
		if err = rows.Scan(&d.ID, &d.ProjectID, &d.Name, &d.Commit, &d.TargetKind, &d.TargetName, &d.LiveCommit, &d.Status, &d.Error, &c, &u, &d.AutomationPaused); err != nil {
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
	if in.TargetKind != "branch" && in.TargetKind != "tag" && in.TargetKind != "commit" {
		http.Error(w, "choose a branch, tag, or commit target", 400)
		return
	}
	if in.TargetKind == "branch" || in.TargetKind == "tag" {
		kind := in.TargetKind
		if in.TargetName == "" || len(in.TargetName) > 255 {
			http.Error(w, "choose a cached Git target", 400)
			return
		}
		commit, refErr := a.cachedProjectRef(r.Context(), in.ProjectID, kind, in.TargetName)
		if refErr != nil {
			http.Error(w, "the selected target is not in the cached refs", 400)
			return
		}
		in.Commit = commit
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
	slot, ok := a.reserveDeploymentSlot()
	if !ok {
		http.Error(w, "the publication queue is full; try again shortly", http.StatusServiceUnavailable)
		return
	}
	defer slot.release()
	job, err := randomID()
	if err != nil {
		http.Error(w, "deployment could not be queued", 500)
		return
	}
	now := a.cfg.Now().UTC()
	tx, err := a.store.Tx(r.Context())
	if err != nil {
		http.Error(w, "deployment could not be saved", 500)
		return
	}
	defer tx.Rollback()
	if _, err = tx.Deployment.Create().SetID(id).SetProjectID(in.ProjectID).SetName(strings.TrimSpace(in.Name)).SetCommitID(in.Commit).SetTargetKind(in.TargetKind).SetTargetName(in.TargetName).SetGeneration(1).SetCreatedAt(now.UnixNano()).SetUpdatedAt(now.UnixNano()).Save(r.Context()); err != nil {
		http.Error(w, "a deployment with this name already exists or could not be saved", 409)
		return
	}
	if _, err = tx.DeploymentJob.Create().SetID(job).SetDeploymentID(id).SetCommitID(in.Commit).SetGeneration(1).SetStatus("queued").SetLog("Publication queued").SetCreatedAt(now.UnixNano()).SetUpdatedAt(now.UnixNano()).Save(r.Context()); err != nil {
		http.Error(w, "deployment could not be queued", 500)
		return
	}
	if err = tx.Commit(); err != nil {
		http.Error(w, "deployment could not be queued", 500)
		return
	}
	slot.enqueue(job)
	writeJSONStatus(w, 201, map[string]any{"id": id, "jobId": job, "status": "queued"})
}

func (a *App) deploymentJobs(w http.ResponseWriter, r *http.Request) {
	if !a.projectAuthorized(w, r, false) {
		return
	}
	id := r.PathValue("id")
	jobs, err := a.store.DeploymentJob.Query().Where(jobmodel.DeploymentIDEQ(id)).Order(ent.Desc(jobmodel.FieldCreatedAt)).Limit(100).All(r.Context())
	if err != nil {
		http.Error(w, "publication history is unavailable", 500)
		return
	}
	out := make([]deploymentJob, 0, len(jobs))
	for _, row := range jobs {
		out = append(out, deploymentJob{ID: row.ID, DeploymentID: row.DeploymentID, Commit: row.CommitID, Status: row.Status, Log: row.Log, CreatedAt: time.Unix(0, row.CreatedAt).UTC(), UpdatedAt: time.Unix(0, row.UpdatedAt).UTC(), Force: row.ForceRedeploy != 0})
	}
	writeJSON(w, out)
}

func (a *App) deployNow(w http.ResponseWriter, r *http.Request) {
	if !a.projectAuthorized(w, r, true) {
		return
	}
	id := r.PathValue("id")
	deployment, err := a.store.Deployment.Get(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if deployment.Status == "deleting" || deployment.Status == "deletion_failed" {
		http.Error(w, "deployment deletion is in progress", http.StatusConflict)
		return
	}
	job, err := a.enqueueDeployment(id, deployment.CommitID, deployment.Generation)
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

func (a *App) hardRedeploy(w http.ResponseWriter, r *http.Request) {
	if !a.projectAuthorized(w, r, true) {
		return
	}
	id := r.PathValue("id")
	slot, ok := a.reserveDeploymentSlot()
	if !ok {
		http.Error(w, "the publication queue is full; try again shortly", http.StatusServiceUnavailable)
		return
	}
	defer slot.release()
	jobID, err := randomID()
	if err != nil {
		http.Error(w, "publication could not be queued", http.StatusInternalServerError)
		return
	}
	now := a.cfg.Now().UnixNano()
	tx, err := a.store.Tx(r.Context())
	if err != nil {
		http.Error(w, "publication could not be queued", http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()
	current, err := tx.Deployment.Get(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if current.Status == "deleting" || current.Status == "deletion_failed" {
		http.Error(w, "deployment deletion is in progress", http.StatusConflict)
		return
	}
	generationUpdate, updateErr := tx.Deployment.Update().Where(deploymentmodel.ID(id), deploymentmodel.Generation(current.Generation), deploymentmodel.StatusNotIn("deleting", "deletion_failed")).AddGeneration(1).SetStatus("queued").SetError("").SetUpdatedAt(now).Save(r.Context())
	if updateErr != nil {
		http.Error(w, "publication could not be queued", http.StatusInternalServerError)
		return
	}
	if generationUpdate != 1 {
		http.Error(w, "deployment target changed; refresh and try again", http.StatusConflict)
		return
	}
	if _, err = tx.DeploymentJob.Create().SetID(jobID).SetDeploymentID(id).SetCommitID(current.CommitID).SetGeneration(current.Generation + 1).SetStatus("queued").SetLog("Hard re-deploy queued; configured target will be refreshed").SetCreatedAt(now).SetUpdatedAt(now).SetForceRedeploy(1).Save(r.Context()); err != nil {
		http.Error(w, "publication could not be queued", http.StatusInternalServerError)
		return
	}
	if err = tx.Commit(); err != nil {
		http.Error(w, "publication could not be queued", http.StatusInternalServerError)
		return
	}
	slot.enqueue(jobID)
	writeJSONStatus(w, http.StatusAccepted, map[string]string{"jobId": jobID, "status": "queued"})
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
	if input.TargetKind == "branch" || input.TargetKind == "tag" {
		kind := input.TargetKind
		if input.TargetName == "" || len(input.TargetName) > 255 {
			http.Error(w, "choose a target from the cached refs", http.StatusBadRequest)
			return
		}
		commit, refErr := a.cachedProjectRef(r.Context(), projectID, kind, input.TargetName)
		if refErr != nil {
			http.Error(w, "choose a target from the cached refs", http.StatusBadRequest)
			return
		}
		input.Commit = commit
	} else if input.TargetKind != "commit" || !validCommitTarget(input.Commit) {
		http.Error(w, "choose a branch, tag, or full commit ID", http.StatusBadRequest)
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
	slot, ok := a.reserveDeploymentSlot()
	if !ok {
		http.Error(w, "the publication queue is full; try again shortly", http.StatusServiceUnavailable)
		return
	}
	defer slot.release()
	jobID, err := randomID()
	if err != nil {
		http.Error(w, "publication could not be queued", 500)
		return
	}
	now := a.cfg.Now().UnixNano()
	tx, err := a.store.Tx(r.Context())
	if err != nil {
		http.Error(w, "publication could not be queued", 500)
		return
	}
	defer tx.Rollback()
	current, err := tx.Deployment.Query().Where(deploymentmodel.ID(id), deploymentmodel.ProjectID(projectID)).Only(r.Context())
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if current.Status == "deleting" || current.Status == "deletion_failed" {
		http.Error(w, "deployment deletion is in progress", http.StatusConflict)
		return
	}
	if current.TargetKind == input.TargetKind && current.TargetName == input.TargetName && current.CommitID == input.Commit {
		http.Error(w, "this deployment already has that target", http.StatusConflict)
		return
	}
	update := tx.Deployment.Update().Where(deploymentmodel.ID(id), deploymentmodel.Generation(current.Generation), deploymentmodel.StatusNotIn("deleting", "deletion_failed")).SetCommitID(input.Commit).SetTargetKind(input.TargetKind).SetTargetName(input.TargetName).AddGeneration(1).SetStatus("queued").SetError("").SetUpdatedAt(now)
	if input.TargetKind != "branch" {
		update.SetAutomationPaused(0)
	}
	res, err := update.Save(r.Context())
	if err != nil {
		http.Error(w, "deployment target could not be saved", 500)
		return
	}
	if res != 1 {
		http.Error(w, "deployment deletion is in progress", http.StatusConflict)
		return
	}
	if _, err = tx.DeploymentJob.Create().SetID(jobID).SetDeploymentID(id).SetCommitID(input.Commit).SetGeneration(current.Generation + 1).SetStatus("queued").SetLog("Commit target changed; publication queued").SetCreatedAt(now).SetUpdatedAt(now).Save(r.Context()); err != nil {
		http.Error(w, "publication could not be queued", 500)
		return
	}
	if err = tx.Commit(); err != nil {
		http.Error(w, "publication could not be queued", 500)
		return
	}
	slot.enqueue(jobID)
	writeJSONStatus(w, http.StatusAccepted, map[string]string{"jobId": jobID, "status": "queued"})
}
