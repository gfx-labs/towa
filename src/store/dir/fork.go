package dir

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"uuid"

	"github.com/gfx-labs/towa/src/filelock"
	"github.com/gfx-labs/towa/src/store"
)

// Fork creates the session meta describes with source's history and stored
// content. The new session has the log format of its source.
//
// The log is copied. Stored content is hard linked: it is named by its hash
// and never rewritten, so a link is a copy that takes no space, and deleting
// either session leaves the other's content in place. On a file system that
// cannot link the two directories the content is copied instead.
func (s *Store) Fork(ctx context.Context, source uuid.UUID, meta store.Meta) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.build(ctx, meta, func(building string) error {
		// build holds the root lock first, as Delete does. Acquiring the
		// source's log first would invert that order.
		log, format, err := s.openLog(source, os.O_RDONLY, filelock.Shared)
		if err != nil {
			return err
		}
		defer log.Close() //nolint:errcheck // Read only: closing drops the lock.
		info, err := log.Stat()
		if err != nil {
			return err
		}
		end, err := format.end(log, info.Size())
		if err != nil {
			return err
		}
		if end != info.Size() {
			return &TornError{Path: log.Name(), Offset: end, Size: info.Size()}
		}
		if err := s.copyFile(ctx, logPath(building, format), io.NewSectionReader(log, 0, end)); err != nil {
			return err
		}
		return s.forkBlobs(ctx, filepath.Join(s.SessionDir(source), blobsName), filepath.Join(building, blobsName))
	})
}

// forkBlobs gives the directory to every blob in from.
func (s *Store) forkBlobs(ctx context.Context, from, to string) error {
	blobs, err := os.ReadDir(from)
	if errors.Is(err, fs.ErrNotExist) {
		// The source never stored any content.
		return nil
	}
	if err != nil {
		return err
	}
	if err := os.Mkdir(to, dirMode); err != nil {
		return err
	}
	link := true
	for _, blob := range blobs {
		if err := ctx.Err(); err != nil {
			return err
		}
		// Only whole content has a digest for a name: a PutBlob that is
		// still writing has a temporary one.
		if _, err := blobName(hashPrefix + blob.Name()); err != nil {
			continue
		}
		src, dst := filepath.Join(from, blob.Name()), filepath.Join(to, blob.Name())
		if link {
			err := os.Link(src, dst)
			if err == nil {
				continue
			}
			if !cannotLink(err) {
				return err
			}
			link = false
		}
		content, err := os.Open(src)
		if err != nil {
			return err
		}
		err = s.copyFile(ctx, dst, content)
		if err = errors.Join(err, content.Close()); err != nil {
			return err
		}
	}
	return s.syncDir(to)
}

// cannotLink reports an error that says the file system will not link the
// two names, whatever the file: they are on two file systems, or it has no
// hard links.
func cannotLink(err error) bool {
	return errors.Is(err, syscall.EXDEV) || errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.ENOTSUP)
}

// copyFile creates path with content and makes it durable.
func (s *Store) copyFile(ctx context.Context, path string, content io.Reader) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, fileMode)
	if err != nil {
		return err
	}
	_, err = io.Copy(f, &ctxReader{ctx: ctx, r: content})
	if err == nil {
		s.syncs.Add(1)
		err = f.Sync()
	}
	return errors.Join(err, f.Close())
}
