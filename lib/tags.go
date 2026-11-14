package lib

import (
	"errors"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
)

const maxAnnotatedTagDepth = 16

// peelTagCommit follows only Git tag objects and accepts the final object only
// when it is a complete commit. The depth and visited set bound hostile cycles.
func peelTagCommit(repo *git.Repository, hash plumbing.Hash) (plumbing.Hash, error) {
	seen := make(map[plumbing.Hash]struct{}, maxAnnotatedTagDepth)
	for depth := 0; depth < maxAnnotatedTagDepth; depth++ {
		if _, ok := seen[hash]; ok {
			return plumbing.ZeroHash, errors.New("tag object cycle")
		}
		seen[hash] = struct{}{}
		if size, sizeErr := repo.Storer.EncodedObjectSize(hash); sizeErr != nil || size > maxGitCommitObjectBytes {
			return plumbing.ZeroHash, errors.New("tag target is missing or too large")
		}
		tag, err := repo.TagObject(hash)
		if err != nil {
			commit, commitErr := repo.CommitObject(hash)
			if commitErr != nil || commit.Hash != hash {
				return plumbing.ZeroHash, errors.New("tag does not resolve to a commit")
			}
			return hash, nil
		}
		if tag.TargetType != plumbing.CommitObject && tag.TargetType != plumbing.TagObject {
			return plumbing.ZeroHash, errors.New("tag does not resolve to a commit")
		}
		hash = tag.Target
	}
	return plumbing.ZeroHash, errors.New("tag nesting exceeds the supported depth")
}
