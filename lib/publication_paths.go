package lib

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"

	"github.com/eslym/deploy-manager/lib/ent"
)

// publicationPaths owns the stable live name and the private names belonging
// to one deployment. Stages remain keyed by deployment ID even for readable
// live names; the live name is bound to the durable deployment record.
type publicationPaths struct {
	root, deploymentID, liveName string
}

func (a *App) publicationPaths(id string) publicationPaths {
	p := publicationPaths{root: a.cfg.DeploymentRoot, deploymentID: id}
	if !validProjectID(id) {
		return p
	}
	row, err := a.store.Deployment.Get(context.Background(), id)
	if err == nil && validStoredPublicationName(row.PublicationName, id) {
		p.liveName = row.PublicationName
	} else if ent.IsNotFound(err) {
		p.liveName = id
	}
	return p
}

var publicationNamePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,99}$`)

func validPublicationName(name string) bool {
	return publicationNamePattern.MatchString(name) && !validProjectID(name)
}

func validStoredPublicationName(name, id string) bool {
	return name == id && validProjectID(id) || validPublicationName(name)
}

func (p publicationPaths) live() string {
	path, err := p.namedPath(p.liveName)
	if err != nil {
		return filepath.Join(p.root, ".invalid-publication-binding")
	}
	return path
}

// namedPath is the only root join for a persisted or proposed live name.
func (p publicationPaths) namedPath(name string) (string, error) {
	if !validProjectID(p.deploymentID) || !validStoredPublicationName(name, p.deploymentID) {
		return "", errors.New("invalid publication name")
	}
	return filepath.Join(p.root, name), nil
}

var errPublicationOccupied = errors.New("publication destination is occupied or reserved")

// available checks both durable reservations and filesystem occupancy. The
// database's unique index and move reservation triggers remain the final
// cross-process guard after this preflight check.
func (a *App) availablePublicationPath(p publicationPaths, name string) (string, error) {
	path, err := p.namedPath(name)
	if err != nil {
		return "", err
	}
	var reservations int
	if err = a.db.QueryRow(`SELECT (SELECT count(*) FROM deployments WHERE publication_name=?) + (SELECT count(*) FROM publication_moves WHERE destination_name=?)`, name, name).Scan(&reservations); err != nil {
		return "", fmt.Errorf("check publication reservations: %w", err)
	}
	if reservations != 0 {
		return "", errPublicationOccupied
	}
	if _, err = os.Lstat(path); err == nil {
		return "", errPublicationOccupied
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("inspect publication destination: %w", err)
	}
	return path, nil
}

func (p publicationPaths) stageName(job string) string {
	return "." + p.deploymentID + ".stage-" + job
}

func (p publicationPaths) stage(job string) string { return filepath.Join(p.root, p.stageName(job)) }

func (p publicationPaths) recordedStage(name, job string) (string, error) {
	if !validProjectID(p.deploymentID) || !validProjectID(job) || name != p.stageName(job) {
		return "", errors.New("publication journal contains an unsafe path or identifier")
	}
	return p.stage(job), nil
}

func (p publicationPaths) removal(name string) (string, error) {
	if !validProjectID(p.deploymentID) || !validStoredPublicationName(p.liveName, p.deploymentID) {
		return "", errors.New("removal journal contains an unsafe path")
	}
	if name == p.liveName {
		return p.live(), nil
	}
	// A stage must have exactly one valid job identifier. No other recorded
	// path can authorize deletion, even if it is inside the publication root.
	const jobSize = 32
	if len(name) < jobSize || !validProjectID(name[len(name)-jobSize:]) {
		return "", errors.New("removal journal contains an unsafe path")
	}
	return p.recordedStage(name, name[len(name)-jobSize:])
}

// checkLive binds an observed real directory to the persisted release. An
// empty live commit owns an absent path, never an arbitrary existing directory.
func (p publicationPaths) checkLive(commit string, want dirIdentity) (dirIdentity, bool, error) {
	if !validProjectID(p.deploymentID) || !validStoredPublicationName(p.liveName, p.deploymentID) {
		return dirIdentity{}, false, errors.New("publication binding is invalid or unavailable")
	}
	actual, err := identity(p.live())
	if os.IsNotExist(err) {
		if commit == "" {
			return dirIdentity{}, false, nil
		}
		return dirIdentity{}, false, errors.New("recorded live publication is missing")
	}
	if err != nil {
		return dirIdentity{}, false, err
	}
	if commit == "" || actual != want {
		return dirIdentity{}, false, errors.New("live publication identity does not match its owner")
	}
	return actual, true, nil
}
