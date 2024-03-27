package dirs

import (
	"io/fs"
	"path"
	"slices"
)

// Only returns an fs.FS that only supports paths within paths.
func Only(fsys fs.FS, paths ...string) fs.FS {
	mapped := make(map[string]struct{}, len(paths))
	for _, p := range paths {
		mapped[p] = struct{}{}
	}

	rdfs, ok := fsys.(fs.ReadDirFS)
	if !ok {
		return &only{
			FS:      fsys,
			allowed: mapped,
		}
	}

	listable := make(map[string]struct{}, len(paths))
	for _, p := range paths {
		next := path.Dir(p)
		for p := p; next != p; p, next = next, path.Dir(p) {
			if _, ok := listable[p]; ok {
				break
			}

			listable[p] = struct{}{}
		}
	}

	return &onlyDirFS{
		ReadDirFS: rdfs,
		allowed:   mapped,
		listable:  listable,
	}
}

func allow(name string, allowed map[string]struct{}) bool {
	if _, ok := allowed[name]; ok {
		return true
	}

	if name == "." || name == "/" {
		return false
	}

	return allow(path.Dir(name), allowed)
}

// TODO: Expose this and use as return type if we like it?
type only struct {
	fs.FS
	allowed  map[string]struct{}
	listable map[string]struct{}
}

func (o *only) Open(name string) (fs.File, error) {
	if !allow(name, o.allowed) {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	}

	return o.FS.Open(name)
}

// TODO: Expose this and use as return type if we like it?
type onlyDirFS struct {
	fs.ReadDirFS
	allowed  map[string]struct{}
	listable map[string]struct{}
}

func (o *onlyDirFS) Open(name string) (fs.File, error) {
	if !allow(name, o.allowed) {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	}

	return o.ReadDirFS.Open(name)
}

func (o *onlyDirFS) ReadDir(name string) ([]fs.DirEntry, error) {
	if _, ok := o.listable[name]; !ok {
		if !allow(name, o.allowed) {
			return nil, &fs.PathError{Op: "readdir", Path: name, Err: fs.ErrNotExist}
		}
	}

	des, err := o.ReadDirFS.ReadDir(name)
	if err != nil {
		return nil, err
	}

	des = slices.DeleteFunc(des, func(de fs.DirEntry) bool {
		if _, ok := o.listable[de.Name()]; ok {
			return false
		}

		return !allow(de.Name(), o.allowed)
	})

	return des, nil
}
