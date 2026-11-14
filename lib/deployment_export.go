package lib

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// exportTrackedTree reads only the commit object database. It does not create a
// native worktree, run checkout filters, or invoke external Git programs.
func exportTrackedTree(ctx context.Context, repo *git.Repository, commit *object.Commit, dest string, limits exportLimits) error {
	if repo == nil || commit == nil {
		return errors.New("cached commit is required")
	}
	deadline := time.Now().Add(limits.timeout)
	tree, err := loadExportTree(repo, commit.TreeHash)
	if err != nil {
		return err
	}
	var total int64
	entries := 0
	seen := map[string]bool{}
	var walk func(*object.Tree, string, int) error
	walk = func(t *object.Tree, prefix string, depth int) error {
		if depth > 256 {
			return errors.New("tracked tree is too deep")
		}
		for _, entry := range t.Entries {
			entries++
			if entries > limits.files {
				return errors.New("export exceeds configured file-count limit")
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
			}
			if time.Now().After(deadline) {
				return context.DeadlineExceeded
			}
			name := entry.Name
			if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\\x00") || name == ".git" {
				return errors.New("tracked tree contains an unsafe path")
			}
			rel := name
			if prefix != "" {
				rel = filepath.ToSlash(filepath.Join(prefix, name))
			}
			if seen[rel] {
				return errors.New("tracked tree contains a duplicate path")
			}
			seen[rel] = true
			target := filepath.Join(dest, filepath.FromSlash(rel))
			if !withinRoot(dest, target) {
				return errors.New("tracked tree path escapes export root")
			}
			switch entry.Mode {
			case filemode.Dir:
				if err := os.Mkdir(target, 0755); err != nil {
					return err
				}
				if err := os.Chmod(target, 0755); err != nil {
					return err
				}
				sub, e := loadExportTree(repo, entry.Hash)
				if e != nil {
					return fmt.Errorf("tracked subtree is missing: %w", e)
				}
				if err = walk(sub, rel, depth+1); err != nil {
					return err
				}
			case filemode.Symlink:
				return errors.New("tracked tree contains symbolic link")
			case filemode.Submodule:
				return errors.New("tracked tree contains submodule")
			case filemode.Regular, filemode.Executable:
				blob, e := repo.BlobObject(entry.Hash)
				if e != nil {
					return fmt.Errorf("tracked blob is missing: %w", e)
				}
				if blob.Size < 0 || blob.Size > limits.bytes-total {
					return errors.New("export exceeds configured byte limit")
				}
				if selectedLFS(repo, commit, rel) {
					return errors.New("tracked attributes select unsupported LFS content")
				}
				r, e := blob.Reader()
				if e != nil {
					return e
				}
				mode := os.FileMode(0644)
				if entry.Mode == filemode.Executable {
					mode = 0755
				}
				out, e := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
				if e != nil {
					r.Close()
					return e
				}
				if e = out.Chmod(mode); e != nil {
					out.Close()
					r.Close()
					return e
				}
				n, copyErr := io.CopyN(out, contextReader{ctx: ctx, deadline: deadline, reader: r}, blob.Size+1)
				closeErr := out.Close()
				readerClose := r.Close()
				if copyErr != nil && copyErr != io.EOF {
					return copyErr
				}
				if n != blob.Size {
					return errors.New("tracked blob size changed during export")
				}
				if closeErr != nil {
					return closeErr
				}
				if readerClose != nil {
					return readerClose
				}
				total += n
			default:
				return errors.New("tracked tree contains unsupported entry mode")
			}
		}
		return nil
	}
	return walk(tree, "", 0)
}

func loadExportTree(repo *git.Repository, hash plumbing.Hash) (*object.Tree, error) {
	size, err := repo.Storer.EncodedObjectSize(hash)
	if err != nil || size < 0 || size > maxGitTreeObjectBytes {
		return nil, errors.New("tracked tree object is missing or exceeds the supported object-size limit")
	}
	return repo.TreeObject(hash)
}

type contextReader struct {
	ctx      context.Context
	deadline time.Time
	reader   io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	if time.Now().After(r.deadline) {
		return 0, context.DeadlineExceeded
	}
	return r.reader.Read(p)
}

func withinRoot(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// Attribute support is deliberately conservative: only straightforward
// tracked rules are recognized; unsupported rules fail closed if they could
// select a pointer. Host-global and info/attributes sources are never read.
func selectedLFS(repo *git.Repository, commit *object.Commit, path string) bool {
	components := strings.Split(filepath.ToSlash(path), "/")
	var filter string
	for i := 0; i < len(components); i++ {
		dir := strings.Join(components[:i], "/")
		tree, e := repo.TreeObject(commit.TreeHash)
		if e != nil {
			return true
		}
		if dir != "" {
			tree, e = tree.Tree(dir)
			if e != nil {
				return true
			}
		}
		f, e := tree.File(".gitattributes")
		if e != nil {
			continue
		}
		if f.Size < 0 || f.Size > 1<<20 {
			return true
		}
		contents, e := f.Contents()
		if e != nil {
			return true
		}
		for _, line := range strings.Split(contents, "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			fields := strings.Fields(line)
			if len(fields) < 2 {
				continue
			}
			pattern := fields[0]
			if strings.ContainsAny(pattern, "\\[]{}") {
				if strings.Contains(strings.Join(fields[1:], " "), "filter=lfs") {
					return true
				}
				continue
			}
			candidate := components[len(components)-1]
			if strings.Contains(pattern, "/") {
				candidate = strings.Join(components[i:], "/")
			}
			matched, matchErr := filepath.Match(pattern, candidate)
			if strings.HasSuffix(pattern, "/") {
				suffix := strings.Join(components[i:], "/")
				matched = strings.HasPrefix(suffix, strings.TrimSuffix(pattern, "/")+"/")
				matchErr = nil
			}
			if matchErr != nil {
				if strings.Contains(strings.Join(fields[1:], " "), "filter=lfs") {
					return true
				}
				continue
			}
			if !matched {
				continue
			}
			for _, attr := range fields[1:] {
				switch {
				case strings.HasPrefix(attr, "filter="):
					filter = strings.TrimPrefix(attr, "filter=")
				case attr == "-filter" || attr == "!filter" || attr == "filter":
					filter = ""
				}
			}
		}
	}
	if filter != "lfs" {
		return false
	}
	// Locate the selected blob in the commit tree and inspect only a bounded
	// prefix. Inline non-pointer data is preserved; malformed pointer-like data
	// is rejected by treating it as selected LFS content.
	tree, e := repo.TreeObject(commit.TreeHash)
	if e != nil {
		return true
	}
	file, e := tree.File(path)
	if e != nil {
		return true
	}
	if file.Size == 0 {
		return false
	}
	r, e := file.Reader()
	if e != nil {
		return true
	}
	var prefix [64]byte
	n, readErr := io.ReadFull(r, prefix[:])
	closeErr := r.Close()
	if readErr != nil && readErr != io.EOF && readErr != io.ErrUnexpectedEOF {
		return true
	}
	if closeErr != nil {
		return true
	}
	return strings.HasPrefix(string(prefix[:n]), "version https://git-lfs")
}
