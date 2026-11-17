package lib

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// BenchmarkExportTrackedTree measures the current full export path, including
// tree traversal, blob reads, and per-file committed-attribute evaluation.
func BenchmarkExportTrackedTree(b *testing.B) {
	root := b.TempDir()
	repoDir := filepath.Join(root, "repo")
	repo, err := git.PlainInit(repoDir, false)
	if err != nil {
		b.Fatal(err)
	}
	worktree, err := repo.Worktree()
	if err != nil {
		b.Fatal(err)
	}
	var attrs string
	for rule := range 128 {
		attrs += fmt.Sprintf("group-%03d/*.dat filter=other\n", rule)
	}
	attrs += "**/*.dat filter=other\n"
	if err := os.WriteFile(filepath.Join(repoDir, ".gitattributes"), []byte(attrs), 0600); err != nil {
		b.Fatal(err)
	}
	if _, err := worktree.Add(".gitattributes"); err != nil {
		b.Fatal(err)
	}
	for i := range 1200 {
		dir := fmt.Sprintf("group-%03d", i%128)
		if err := os.MkdirAll(filepath.Join(repoDir, dir), 0700); err != nil {
			b.Fatal(err)
		}
		name := filepath.Join(dir, fmt.Sprintf("asset-%04d.dat", i))
		content := []byte(fmt.Sprintf("representative static content %04d\n", i))
		if err := os.WriteFile(filepath.Join(repoDir, name), content, 0600); err != nil {
			b.Fatal(err)
		}
		if _, err := worktree.Add(name); err != nil {
			b.Fatal(err)
		}
	}
	commitID, err := worktree.Commit("benchmark release tree", &git.CommitOptions{
		Author: &object.Signature{Name: "benchmark", Email: "benchmark@example.test", When: time.Unix(1, 0).UTC()},
	})
	if err != nil {
		b.Fatal(err)
	}
	commit, err := repo.CommitObject(commitID)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.SetBytes(1200 * 35)
	b.ResetTimer()
	for i := range b.N {
		dest := filepath.Join(root, fmt.Sprintf("export-%d", i))
		if err := os.Mkdir(dest, 0700); err != nil {
			b.Fatal(err)
		}
		if err := exportTrackedTree(context.Background(), repo, commit, dest, exportLimits{
			files: 2000, bytes: 2 << 20, timeout: time.Minute,
		}); err != nil {
			b.Fatal(err)
		}
	}
}
