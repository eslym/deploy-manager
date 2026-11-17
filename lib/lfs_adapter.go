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

type preparedAttributeFile struct {
	lines []gitattr.Line
	err   error
}

// preparedAttributes retains source text and parses each applicable tracked
// attribute file only when first needed during this export.
type preparedAttributes struct {
	files  map[string]string
	parsed map[string]preparedAttributeFile
	work   int
}

func prepareAttributes(files map[string]string) *preparedAttributes {
	return &preparedAttributes{files: files, parsed: make(map[string]preparedAttributeFile)}
}

func (p *preparedAttributes) parse(name, contents string) ([]gitattr.Line, error) {
	cached, ok := p.parsed[name]
	if !ok {
		for _, rawLine := range strings.Split(contents, "\n") {
			if strings.HasPrefix(strings.TrimSpace(rawLine), "!") {
				cached.err = errors.New("committed attributes contain a forbidden negative pattern")
				break
			}
		}
		if cached.err == nil {
			cached.lines, _, cached.err = gitattr.ParseLines(strings.NewReader(contents))
			if cached.err != nil {
				cached.err = errors.New("committed attributes are malformed")
			}
		}
		p.parsed[name] = cached
	}
	return cached.lines, cached.err
}

func (p *preparedAttributes) effective(file string) (bool, error) {
	files := p.files
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
	macros := map[string][]*gitattr.Attr{
		"binary": {{K: "diff", V: "false"}, {K: "merge", V: "false"}, {K: "text", V: "false"}},
	}
	filter := ""
	expansion := 0
	for _, dir := range levels {
		name := ".gitattributes"
		if dir != "" {
			name = path.Join(dir, name)
		}
		contents, ok := files[name]
		if !ok {
			continue
		}
		lines, err := p.parse(name, contents)
		if err != nil {
			return false, err
		}
		if dir == "" {
			for _, line := range lines {
				if macro, ok := line.(gitattr.MacroLine); ok {
					macros[macro.Macro()] = macro.Attrs()
				}
			}
		}
		for _, raw := range lines {
			line, ok := raw.(gitattr.PatternLine)
			if !ok {
				continue
			}
			p.work++
			if p.work > maxLFSAttributeWork {
				return false, errors.New("committed attribute matching exceeds the work limit")
			}
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
			attrs, err := expandGitAttributes(line.Attrs(), macros, nil, &expansion)
			if err != nil {
				return false, err
			}
			for _, attr := range attrs {
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

const maxLFSAttributeWork = 10_000_000
const maxAttributeExpansions = 10_000

// Git expands set macros recursively, preserving assignment order and the
// macro's own attribute assignment. Unset and unspecified macro names are
// ordinary attributes, not invocations. Bound work and reject cyclic graphs.
func expandGitAttributes(attrs []*gitattr.Attr, macros map[string][]*gitattr.Attr, stack map[string]bool, steps *int) ([]*gitattr.Attr, error) {
	var out []*gitattr.Attr
	for _, attr := range attrs {
		(*steps)++
		if *steps > maxAttributeExpansions {
			return nil, errors.New("committed attributes exceed expansion limit")
		}
		if attr.V == "true" && !attr.Unspecified {
			if body, ok := macros[attr.K]; ok {
				if stack[attr.K] {
					return nil, errors.New("committed attributes contain a macro cycle")
				}
				if stack == nil {
					stack = make(map[string]bool)
				}
				if len(stack) >= maxAttributeExpansionDepth {
					return nil, errors.New("committed attribute macro nesting exceeds the limit")
				}
				stack[attr.K] = true
				expanded, err := expandGitAttributes(body, macros, stack, steps)
				delete(stack, attr.K)
				if err != nil {
					return nil, err
				}
				out = append(out, expanded...)
			}
		}
		out = append(out, attr)
	}
	return out, nil
}

const maxAttributeExpansionDepth = 256

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
