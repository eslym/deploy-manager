package lib

import (
	"bytes"
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
func exportTrackedTree(ctx context.Context, repo *git.Repository, commit *object.Commit, dest string, limits exportLimits, hydrators ...*lfsHydrator) error {
	if repo == nil || commit == nil {
		return errors.New("cached commit is required")
	}
	deadline := time.Now().Add(limits.timeout)
	tree, err := loadExportTree(repo, commit.TreeHash)
	if err != nil {
		return err
	}
	attributes, err := committedAttributes(repo, tree, limits.files, min(limits.bytes, int64(16<<20)))
	if err != nil {
		return err
	}
	prepared := prepareAttributes(attributes)
	var hydrator *lfsHydrator
	if len(hydrators) > 0 {
		hydrator = hydrators[0]
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
				selected, attrErr := prepared.effective(rel)
				if attrErr != nil {
					return attrErr
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
				var n int64
				var copyErr error
				expectedSize := blob.Size
				if selected {
					var header [maxLFSPointerBytes]byte
					headerN, headerErr := io.ReadFull(contextReader{ctx: ctx, deadline: deadline, reader: r}, header[:])
					if headerErr != nil && headerErr != io.EOF && headerErr != io.ErrUnexpectedEOF {
						_ = out.Close()
						_ = r.Close()
						return headerErr
					}
					data := header[:headerN]
					if looksLikeLFSPointer(data) {
						ptr, parseErr := parseLFSPointer(bytes.NewReader(data), int64(blob.Size))
						if parseErr != nil {
							_ = out.Close()
							_ = r.Close()
							return parseErr
						}
						if hydrator == nil {
							_ = out.Close()
							_ = r.Close()
							return errors.New("selected LFS content has no approved endpoint")
						}
						if ptr.Size > limits.bytes-total {
							_ = out.Close()
							_ = r.Close()
							return errors.New("export exceeds configured byte limit")
						}
						expectedSize = ptr.Size
						cachePath, hydrateErr := hydrator.hydrate(ctx, ptr)
						if hydrateErr != nil {
							_ = out.Close()
							_ = r.Close()
							return hydrateErr
						}
						if !validCachedLFS(cachePath, ptr) {
							_ = out.Close()
							_ = r.Close()
							return errors.New("cached LFS payload failed integrity verification")
						}
						payload, openErr := openLFSCacheFile(cachePath)
						if openErr != nil {
							_ = out.Close()
							_ = r.Close()
							return openErr
						}
						n, copyErr = io.CopyN(out, contextReader{ctx: ctx, deadline: deadline, reader: payload}, ptr.Size+1)
						payloadClose := payload.Close()
						if payloadClose != nil && copyErr == nil {
							copyErr = payloadClose
						}
						if copyErr == io.EOF && n == ptr.Size {
							copyErr = nil
						}
						if copyErr == nil && n != ptr.Size {
							copyErr = errors.New("hydrated LFS payload size changed")
						}
					} else {
						if blob.Size > limits.bytes-total {
							_ = out.Close()
							_ = r.Close()
							return errors.New("export exceeds configured byte limit")
						}
						written, writeErr := out.Write(data)
						n = int64(written)
						if writeErr != nil {
							copyErr = writeErr
						} else {
							var tail int64
							tail, copyErr = io.Copy(out, contextReader{ctx: ctx, deadline: deadline, reader: r})
							n += tail
						}
					}
				} else {
					n, copyErr = io.CopyN(out, contextReader{ctx: ctx, deadline: deadline, reader: r}, blob.Size+1)
				}
				closeErr := out.Close()
				readerClose := r.Close()
				if copyErr != nil && copyErr != io.EOF {
					return copyErr
				}
				if n != expectedSize {
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

func committedAttributes(repo *git.Repository, root *object.Tree, maxEntries int, maxAttributeBytes int64) (map[string]string, error) {
	files := make(map[string]string)
	count := 0
	var totalAttributeBytes int64
	totalAttributeRules := 0
	var walk func(*object.Tree, string, int) error
	walk = func(tree *object.Tree, prefix string, depth int) error {
		if depth > 256 {
			return errors.New("tracked tree is too deep")
		}
		for _, entry := range tree.Entries {
			count++
			if count > maxEntries {
				return errors.New("export exceeds configured file-count limit")
			}
			name := entry.Name
			if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\\x00") {
				return errors.New("tracked tree contains an unsafe path")
			}
			rel := name
			if prefix != "" {
				rel = filepath.ToSlash(filepath.Join(prefix, name))
			}
			switch entry.Mode {
			case filemode.Dir:
				sub, err := loadExportTree(repo, entry.Hash)
				if err != nil {
					return fmt.Errorf("tracked subtree is missing: %w", err)
				}
				if err = walk(sub, rel, depth+1); err != nil {
					return err
				}
			case filemode.Regular, filemode.Executable:
				if entry.Name != ".gitattributes" {
					continue
				}
				blob, err := repo.BlobObject(entry.Hash)
				if err != nil || blob.Size < 0 || blob.Size > 1<<20 {
					return errors.New("committed .gitattributes is missing or too large")
				}
				if blob.Size > maxAttributeBytes-totalAttributeBytes {
					return errors.New("committed attributes exceed the configured parsing limit")
				}
				reader, err := blob.Reader()
				if err != nil {
					return err
				}
				contents, readErr := io.ReadAll(io.LimitReader(reader, (1<<20)+1))
				closeErr := reader.Close()
				if readErr != nil || closeErr != nil || int64(len(contents)) != blob.Size || len(contents) > 1<<20 {
					return errors.New("committed .gitattributes could not be read safely")
				}
				totalAttributeRules += strings.Count(string(contents), "\n") + 1
				if totalAttributeRules > 100_000 {
					return errors.New("committed attributes exceed the rule-count limit")
				}
				files[rel] = string(contents)
				totalAttributeBytes += blob.Size
			default:
				// The main export pass provides the user-visible rejection for
				// symlinks and submodules; they cannot supply attributes.
			}
		}
		return nil
	}
	if err := walk(root, "", 0); err != nil {
		return nil, err
	}
	return files, nil
}

func looksLikeLFSPointer(data []byte) bool {
	if !bytes.HasPrefix(data, []byte("version ")) {
		return false
	}
	return bytes.Contains(data, []byte("git-lfs")) || bytes.Contains(data, []byte("hawser")) || bytes.Contains(data, []byte("git-media")) || bytes.Contains(data, []byte("oid sha256:"))
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
