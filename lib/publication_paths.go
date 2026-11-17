package lib

import (
	"errors"
	"os"
	"path/filepath"
)

// publicationPaths owns the stable live name and the private names belonging
// to one deployment. The v1 live name is the deployment ID; it is intentionally
// not derived from the display name, Git target, job, or current release. A
// future path-name migration can change this one binding without relocating
// existing ID-named publications or changing how job stages are identified.
type publicationPaths struct {
	root, deploymentID, liveName string
}

func (a *App) publicationPaths(id string) publicationPaths {
	return publicationPaths{root: a.cfg.DeploymentRoot, deploymentID: id, liveName: id}
}

func (p publicationPaths) live() string { return filepath.Join(p.root, p.liveName) }

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
	if !validProjectID(p.deploymentID) {
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
