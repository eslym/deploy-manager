package lib

import (
	"testing"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/storage/memory"
)

func TestPeelTagCommitAcceptsLightweightAndNestedAnnotatedTags(t *testing.T) {
	repo, err := git.Init(memory.NewStorage(), nil)
	if err != nil {
		t.Fatal(err)
	}
	blob := &plumbing.MemoryObject{}
	blob.SetType(plumbing.BlobObject)
	blob.SetSize(4)
	_, _ = blob.Write([]byte("site"))
	blobHash, err := repo.Storer.SetEncodedObject(blob)
	if err != nil {
		t.Fatal(err)
	}
	tree := &object.Tree{Entries: []object.TreeEntry{{Name: "index.html", Mode: 0100644, Hash: blobHash}}}
	treeObject := repo.Storer.NewEncodedObject()
	if err := tree.Encode(treeObject); err != nil {
		t.Fatal(err)
	}
	treeHash, err := repo.Storer.SetEncodedObject(treeObject)
	if err != nil {
		t.Fatal(err)
	}
	commit := &object.Commit{TreeHash: treeHash, Author: object.Signature{Name: "test", Email: "test@example.com"}, Committer: object.Signature{Name: "test", Email: "test@example.com"}, Message: "initial"}
	commitObject := repo.Storer.NewEncodedObject()
	if err := commit.Encode(commitObject); err != nil {
		t.Fatal(err)
	}
	commitHash, err := repo.Storer.SetEncodedObject(commitObject)
	if err != nil {
		t.Fatal(err)
	}
	light, err := peelTagCommit(repo, commitHash)
	if err != nil || light != commitHash {
		t.Fatalf("lightweight tag peeled to %s, %v", light, err)
	}
	inner := &object.Tag{Target: commitHash, TargetType: plumbing.CommitObject, Name: "inner", Tagger: object.Signature{Name: "test", Email: "test@example.com"}}
	innerObject := repo.Storer.NewEncodedObject()
	if err := inner.Encode(innerObject); err != nil {
		t.Fatal(err)
	}
	innerHash, err := repo.Storer.SetEncodedObject(innerObject)
	if err != nil {
		t.Fatal(err)
	}
	outer := &object.Tag{Target: innerHash, TargetType: plumbing.TagObject, Name: "outer", Tagger: object.Signature{Name: "test", Email: "test@example.com"}}
	outerObject := repo.Storer.NewEncodedObject()
	if err := outer.Encode(outerObject); err != nil {
		t.Fatal(err)
	}
	outerHash, err := repo.Storer.SetEncodedObject(outerObject)
	if err != nil {
		t.Fatal(err)
	}
	peeled, err := peelTagCommit(repo, outerHash)
	if err != nil || peeled != commitHash {
		t.Fatalf("nested annotated tag peeled to %s, %v", peeled, err)
	}
	if _, err := peelTagCommit(repo, blobHash); err == nil {
		t.Fatal("tag pointing to a blob was accepted as a commit")
	}
}
