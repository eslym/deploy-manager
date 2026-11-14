package lib

import (
	"bytes"
	"errors"
	"io"
	"path"
	"strings"

	"github.com/git-lfs/git-lfs/v3/git/gitattr"
	gitlfs "github.com/git-lfs/git-lfs/v3/lfs"
)

// lfsPointer is the application-owned representation of the small subset of
// Git LFS pointers that this publisher can hydrate. Keep upstream types behind
// this adapter so protocol policy stays local to the application.
type lfsPointer struct {
	OID  string
	Size int64
}

// effectiveLFSAttribute applies only committed .gitattributes inputs. Source
// files are processed from the repository root toward the file, and rules are
// reduced in order so later rules and deeper files take precedence.
func effectiveLFSAttribute(files map[string]string, file string) (bool, error) {
	file = path.Clean(strings.TrimPrefix(file, "/"))
	if file == "." || strings.HasPrefix(file, "../") {
		return false, errors.New("invalid tracked path for attribute lookup")
	}
	parts := strings.Split(path.Dir(file), "/")
	if parts[0] == "." {
		parts = nil
	}
	levels := []string{""}
	for i := range parts {
		levels = append(levels, path.Join(parts[:i+1]...))
	}
	macros := gitattr.NewMacroProcessor()
	filter := ""
	for _, dir := range levels {
		name := ".gitattributes"
		if dir != "" {
			name = path.Join(dir, name)
		}
		contents, ok := files[name]
		if !ok {
			continue
		}
		lines, _, err := gitattr.ParseLines(strings.NewReader(contents))
		if err != nil {
			return false, errors.New("committed attributes are malformed")
		}
		processed := macros.ProcessLines(lines, dir == "")
		for _, line := range processed {
			rel := file
			if dir != "" {
				if rel == dir {
					rel = path.Base(file)
				} else if strings.HasPrefix(rel, dir+"/") {
					rel = strings.TrimPrefix(rel, dir+"/")
				} else {
					continue
				}
			}
			if !line.Pattern().Match(rel) {
				continue
			}
			for _, attr := range line.Attrs() {
				if attr.K != "filter" {
					continue
				}
				if attr.Unspecified || attr.V == "false" {
					filter = ""
				} else {
					filter = attr.V
				}
			}
		}
	}
	return filter == "lfs", nil
}

const maxLFSPointerBytes = 1024

func parseLFSPointer(r io.Reader, size int64) (lfsPointer, error) {
	if size < 0 || size >= maxLFSPointerBytes {
		return lfsPointer{}, errors.New("LFS pointer exceeds the supported size")
	}
	b, err := io.ReadAll(io.LimitReader(r, maxLFSPointerBytes))
	if err != nil {
		return lfsPointer{}, err
	}
	if int64(len(b)) != size {
		return lfsPointer{}, errors.New("LFS pointer size changed while reading")
	}
	p, err := gitlfs.DecodePointer(bytes.NewReader(b))
	if err != nil || p == nil || !p.Canonical || len(p.Extensions) != 0 || p.Size < 0 {
		return lfsPointer{}, errors.New("malformed or unsupported LFS pointer")
	}
	return lfsPointer{OID: p.Oid, Size: p.Size}, nil
}
