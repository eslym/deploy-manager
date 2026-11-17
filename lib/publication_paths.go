package lib

import (
	"context"
	"errors"
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
	if !validProjectID(p.deploymentID) || !validStoredPublicationName(p.liveName, p.deploymentID) {
		return filepath.Join(p.root, ".invalid-publication-binding")
	}
	return filepath.Join(p.root, p.liveName)
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
