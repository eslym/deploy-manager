package lib

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/go-git/go-billy/v5"
	"github.com/go-git/go-billy/v5/osfs"
	"github.com/go-git/go-git/v5/plumbing/cache"
	"github.com/go-git/go-git/v5/storage/filesystem"
)

var errGitCacheLimit = errors.New("repository cache write exceeds the configured storage limit")

// boundedGitFS enforces a logical-byte ceiling at the filesystem write boundary.
// Fetch may expand or re-encode objects; the HTTP transfer limit alone is not a
// bound on bytes written by the object database.
type boundedGitFS struct {
	billy.Filesystem
	root  string
	limit int64
	mu    sync.Mutex
}

func newBoundedGitFS(root string, limit int64) *boundedGitFS {
	return &boundedGitFS{Filesystem: osfs.New(root), root: root, limit: limit}
}

func (f *boundedGitFS) Create(name string) (billy.File, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	file, err := f.Filesystem.Create(name)
	if err != nil {
		return nil, err
	}
	return &boundedGitFile{File: file, fs: f, name: name}, nil
}

func (f *boundedGitFS) OpenFile(name string, flag int, perm os.FileMode) (billy.File, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	file, err := f.Filesystem.OpenFile(name, flag, perm)
	if err != nil {
		return nil, err
	}
	return &boundedGitFile{File: file, fs: f, name: name}, nil
}

func (f *boundedGitFS) TempFile(dir, prefix string) (billy.File, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	file, err := f.Filesystem.TempFile(dir, prefix)
	if err != nil {
		return nil, err
	}
	name := file.Name()
	if rel, relErr := filepath.Rel(f.root, name); relErr == nil {
		name = rel
	}
	return &boundedGitFile{File: file, fs: f, name: name}, nil
}

func (f *boundedGitFS) Rename(oldname, newname string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.Filesystem.Rename(oldname, newname)
}

func (f *boundedGitFS) Remove(name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.Filesystem.Remove(name)
}

type boundedGitFile struct {
	billy.File
	fs   *boundedGitFS
	name string
}

func (w *boundedGitFile) Write(p []byte) (int, error) {
	w.fs.mu.Lock()
	defer w.fs.mu.Unlock()
	used, err := directorySize(w.fs.root, w.fs.limit)
	if err != nil {
		return 0, errGitCacheLimit
	}
	info, statErr := w.fs.Filesystem.Stat(w.name)
	var oldSize int64
	if statErr == nil {
		oldSize = info.Size()
	} else if !os.IsNotExist(statErr) {
		return 0, statErr
	}
	position, err := w.File.Seek(0, io.SeekCurrent)
	if err != nil {
		return 0, err
	}
	end := position + int64(len(p))
	if end > oldSize && used+end-oldSize > w.fs.limit {
		return 0, errGitCacheLimit
	}
	return w.File.Write(p)
}

func (w *boundedGitFile) Truncate(size int64) error {
	w.fs.mu.Lock()
	defer w.fs.mu.Unlock()
	info, err := w.fs.Filesystem.Stat(w.name)
	if err != nil {
		return err
	}
	if size > info.Size() {
		used, sizeErr := directorySize(w.fs.root, w.fs.limit)
		if sizeErr != nil || used+size-info.Size() > w.fs.limit {
			return errGitCacheLimit
		}
	}
	return w.File.Truncate(size)
}

func boundedStorage(root string, limit int64) *filesystem.Storage {
	return filesystem.NewStorage(newBoundedGitFS(root, limit), cache.NewObjectLRUDefault())
}

// copyGitCache creates an isolated staging copy while bounding source bytes and
// destination bytes. Symlinks are rejected rather than followed.
func copyGitCache(source, destination string, limit int64) error {
	var copied int64
	err := filepath.WalkDir(source, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		target := destination
		if rel != "." {
			target = filepath.Join(destination, rel)
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return errors.New("repository cache contains an unsupported symbolic link")
		}
		if info.IsDir() {
			return os.MkdirAll(target, info.Mode().Perm())
		}
		if !info.Mode().IsRegular() {
			return errors.New("repository cache contains an unsupported file")
		}
		if info.Size() > limit-copied {
			return errGitCacheLimit
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, info.Mode().Perm())
		if err != nil {
			_ = in.Close()
			return err
		}
		n, copyErr := io.CopyN(out, in, info.Size()+1)
		closeOutErr, closeInErr := out.Close(), in.Close()
		if copyErr != nil && copyErr != io.EOF {
			return copyErr
		}
		if n != info.Size() {
			return errors.New("repository cache changed while staging")
		}
		if closeOutErr != nil {
			return closeOutErr
		}
		if closeInErr != nil {
			return closeInErr
		}
		copied += n
		return nil
	})
	return err
}
