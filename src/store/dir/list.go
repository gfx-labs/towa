package dir

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
	"uuid"

	"github.com/gfx-labs/towa/src/store"
	"github.com/gfx-labs/towa/src/support/filelock"
)

// indexDir holds only derived data. A moved session needs none of it.
const (
	indexDir     = ".index"
	childrenName = "children.json"
)

// childrenIndex maps a parent id to its children. RootModified is the
// root's modification time when it was built: a directory arriving,
// leaving, or being renamed makes it stale. ParentID never changes.
type childrenIndex struct {
	RootModified time.Time              `json:"root_modified"`
	Children     map[string][]uuid.UUID `json:"children"`
}

// lockRoot orders changes to the root namespace, including making an
// index. It is never acquired while holding a session log lock.
func (s *Store) lockRoot(how filelock.Mode) (*os.File, error) {
	f, err := os.Open(s.root)
	if err != nil {
		return nil, err
	}
	if err := filelock.Lock(f, how); err != nil {
		return nil, errors.Join(err, f.Close())
	}
	return f, nil
}

// List returns matching sessions in id order, oldest first. Filters apply
// before the limit; After is exclusive, even if its session was deleted.
func (s *Store) List(ctx context.Context, filter store.Filter) ([]store.Meta, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if filter.Limit < 0 {
		return nil, fmt.Errorf("store/dir: list limit %d is negative", filter.Limit)
	}
	switch filter.Archived {
	case store.ArchivedAny, store.ArchivedExclude, store.ArchivedOnly:
	default:
		return nil, fmt.Errorf("store/dir: archived filter %d is not valid", filter.Archived)
	}
	how := filelock.Shared
	if filter.ParentID != uuid.Nil() {
		how = filelock.Exclusive
	}
	root, err := s.lockRoot(how)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer root.Close() //nolint:errcheck // Closing drops the directory lock.
	var ids []uuid.UUID
	if filter.ParentID != uuid.Nil() {
		ids, err = s.children(ctx, root, filter.ParentID)
	} else {
		ids, err = s.ids(ctx)
	}
	if err != nil {
		return nil, err
	}
	var metas []store.Meta
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if id.Compare(filter.After) <= 0 {
			continue
		}
		// Filter from meta.json before opening the log: an excluded session
		// need not have its last record read to get its Updated time.
		if filter.Cwd != "" || filter.Archived != store.ArchivedAny {
			meta, err := s.readMeta(id)
			if err != nil {
				return nil, err
			}
			if filter.Cwd != "" && meta.Cwd != filter.Cwd {
				continue
			}
			if filter.Archived == store.ArchivedExclude && meta.Archived {
				continue
			}
			if filter.Archived == store.ArchivedOnly && !meta.Archived {
				continue
			}
		}
		v, err := s.snapshot(id)
		if err != nil {
			return nil, err
		}
		// SetMeta does not need the root lock and may have run between the two
		// reads. Use the same filter on the snapshot too.
		if filter.Cwd != "" && v.meta.Cwd != filter.Cwd {
			continue
		}
		if filter.Archived == store.ArchivedExclude && v.meta.Archived {
			continue
		}
		if filter.Archived == store.ArchivedOnly && !v.meta.Archived {
			continue
		}
		metas = append(metas, v.meta)
		if filter.Limit > 0 && len(metas) == filter.Limit {
			break
		}
	}
	return metas, nil
}

// ids returns the ids named by directories under the root, sorted. The
// caller holds the root lock, so no Create or Delete changes it meanwhile.
func (s *Store) ids(ctx context.Context) ([]uuid.UUID, error) {
	names, err := os.ReadDir(s.root)
	if err != nil {
		return nil, err
	}
	ids := make([]uuid.UUID, 0, len(names))
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if name.Name() == indexDir || leftover(name.Name()) {
			continue
		}
		id, err := uuid.Parse(name.Name())
		if err != nil || id.String() != name.Name() || !name.IsDir() {
			return nil, &CorruptError{Path: filepath.Join(s.root, name.Name()), Err: errors.New("not a session directory")}
		}
		ids = append(ids, id)
	}
	return ids, nil
}

// children uses the derived index when it describes the current root,
// otherwise builds it from the session directories. Root is locked
// exclusively until the caller has finished using it.
func (s *Store) children(ctx context.Context, root *os.File, parent uuid.UUID) ([]uuid.UUID, error) {
	path := filepath.Join(s.root, indexDir)
	if err := os.Mkdir(path, dirMode); err != nil && !errors.Is(err, fs.ErrExist) {
		return nil, err
	}
	info, err := root.Stat()
	if err != nil {
		return nil, err
	}
	file := filepath.Join(path, childrenName)
	data, err := os.ReadFile(file)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	var index childrenIndex
	if err == nil {
		if err := json.Unmarshal(data, &index); err != nil {
			return nil, &CorruptError{Path: file, Err: err}
		}
		written, err := os.Stat(file)
		if err != nil {
			return nil, err
		}
		// A change in the same timestamp tick as the build leaves the root's
		// time as it was. Only an index written in a later tick proves that
		// no such change followed it.
		if index.RootModified.Equal(info.ModTime()) && written.ModTime().After(info.ModTime()) {
			return index.Children[parent.String()], nil
		}
	}
	index = childrenIndex{RootModified: info.ModTime(), Children: map[string][]uuid.UUID{}}
	ids, err := s.ids(ctx)
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		meta, err := s.readMeta(id)
		if err != nil {
			return nil, err
		}
		if meta.ParentID != uuid.Nil() {
			key := meta.ParentID.String()
			index.Children[key] = append(index.Children[key], id)
		}
	}
	data, err = json.Marshal(index)
	if err != nil {
		return nil, err
	}
	if err := s.replaceFile(path, childrenName, data); err != nil {
		return nil, err
	}
	return index.Children[parent.String()], nil
}
