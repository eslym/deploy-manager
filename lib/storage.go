package lib

import "fmt"

// readyStorage owns prepared persistence. Obtaining it starts no recovery,
// cleanup, grants or workers. Only a successful preparation can release normal
// startup. Future singleton/maintenance policy belongs before this release,
// not in domain constructors or HTTP handlers.
type readyStorage struct {
	app *App
}

// GrantIssuer is command-scoped persistence for setup and account recovery.
// It exposes no HTTP handler or daemon runtime and may be closed independently
// of a running daemon. Version/maintenance coordination belongs at preparation.
type GrantIssuer struct {
	app *App
}

func NewGrantIssuer(cfg Config) (*GrantIssuer, error) {
	storage, err := prepareStorage(cfg)
	if err != nil {
		return nil, err
	}
	return &GrantIssuer{app: storage.app}, nil
}

func (g *GrantIssuer) Close() error { return g.app.Close() }
func (g *GrantIssuer) IssueRegistrationGrant() (string, error) {
	return g.app.IssueRegistrationGrant()
}
func (g *GrantIssuer) IssueRecoveryGrant() (string, error) { return g.app.IssueRecoveryGrant() }
func (g *GrantIssuer) RegistrationURL(base, token string) (string, error) {
	return g.app.RegistrationURL(base, token)
}
func (g *GrantIssuer) RecoveryURL(base, token string) (string, error) {
	return g.app.RecoveryURL(base, token)
}

// New prepares storage before performing normal application recovery/startup.
// Failure returns no usable application and releases opened resources.
func New(cfg Config) (*App, error) {
	storage, err := prepareStorage(cfg)
	if err != nil {
		return nil, err
	}
	return storage.start()
}

func (s *readyStorage) start() (*App, error) {
	app := s.app
	cfg, db := app.cfg, app.db
	projects, err := newProjectManager(db, cfg.DatabasePath+".projects", cfg.GitTimeout, cfg.GitMaxCacheBytes)
	if err != nil {
		_ = app.Close()
		return nil, err
	}
	projects.maxTransferBytes, projects.maxCacheBytes, projects.maxRefs, projects.maxAdvertisementBytes = cfg.GitMaxTransferBytes, cfg.GitMaxCacheBytes, cfg.GitMaxRefs, cfg.GitMaxAdvertisementBytes
	app.projects = projects
	if err := app.initDeployments(); err != nil {
		_ = app.Close()
		return nil, err
	}
	if app.cfg.beforeRestoreDeploymentJobs != nil {
		if err := app.cfg.beforeRestoreDeploymentJobs(db); err != nil {
			_ = app.Close()
			return nil, fmt.Errorf("prepare deployment queue recovery: %w", err)
		}
	}
	restoredJobs, err := restoreQueuedDeploymentJobs(db, deploymentQueueCapacity)
	if err != nil {
		_ = app.Close()
		return nil, fmt.Errorf("restore accepted deployment work: %w", err)
	}
	app.retryProjectDeletions()
	app.startDeploymentWorkers(restoredJobs)
	app.deployWG.Add(1)
	go app.publicationCleanupLoop()
	return app, nil
}
