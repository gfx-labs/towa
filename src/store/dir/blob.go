package dir

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"uuid"

	"github.com/gfx-labs/acpharness/src/store"
	"github.com/gfx-labs/acpharness/src/transcript"
)

// hashPrefix starts every hash, as transcript.Stored defines it.
const hashPrefix = "sha256:"

// blobName returns the file name of a hash, which is its hex digest. Only
// a well-formed hash becomes a path.
func blobName(hash string) (string, error) {
	digest, ok := strings.CutPrefix(hash, hashPrefix)
	if !ok || len(digest) != hex.EncodedLen(sha256.Size) || strings.ToLower(digest) != digest {
		return "", fmt.Errorf("store/dir: %q is not %q and a lowercase hex digest", hash, hashPrefix)
	}
	if _, err := hex.DecodeString(digest); err != nil {
		return "", fmt.Errorf("store/dir: %q is not %q and a lowercase hex digest", hash, hashPrefix)
	}
	return digest, nil
}

// PutBlob stores content in the session's blobs directory under its hash.
func (s *Store) PutBlob(ctx context.Context, id uuid.UUID, mimeType string, content io.Reader) (transcript.Stored, error) {
	if err := ctx.Err(); err != nil {
		return transcript.Stored{}, err
	}
	// The shared lock keeps Delete away while the content is written.
	log, _, err := s.openLog(id, os.O_RDONLY, syscall.LOCK_SH)
	if err != nil {
		return transcript.Stored{}, err
	}
	defer log.Close() //nolint:errcheck // Read only: closing drops the lock and cannot lose data.

	session := s.SessionDir(id)
	blobs := filepath.Join(session, blobsName)
	if err := os.Mkdir(blobs, dirMode); err == nil {
		if err := s.syncDir(session); err != nil {
			return transcript.Stored{}, err
		}
	} else if !errors.Is(err, fs.ErrExist) {
		return transcript.Stored{}, err
	}
	// The hash is known only after the content is read, so it is written
	// under a temporary name first.
	tmp, err := os.CreateTemp(blobs, ".put-*")
	if err != nil {
		return transcript.Stored{}, err
	}
	stored, err := s.putBlob(ctx, blobs, tmp, content)
	if err != nil {
		return transcript.Stored{}, errors.Join(err, os.Remove(tmp.Name()))
	}
	stored.MimeType = mimeType
	return stored, nil
}

func (s *Store) putBlob(ctx context.Context, blobs string, tmp *os.File, content io.Reader) (transcript.Stored, error) {
	sum := sha256.New()
	size, err := io.Copy(io.MultiWriter(tmp, sum), &ctxReader{ctx: ctx, r: content})
	s.written.Add(size)
	if err != nil {
		return transcript.Stored{}, errors.Join(err, tmp.Close())
	}
	digest := hex.EncodeToString(sum.Sum(nil))
	stored := transcript.Stored{Hash: hashPrefix + digest, Size: size}
	final := filepath.Join(blobs, digest)
	if _, err := os.Lstat(final); err == nil {
		// The session already holds this content, and it was made durable
		// when it was first stored.
		return stored, errors.Join(tmp.Close(), os.Remove(tmp.Name()))
	} else if !errors.Is(err, fs.ErrNotExist) {
		return transcript.Stored{}, errors.Join(err, tmp.Close())
	}
	s.syncs.Add(1)
	if err := tmp.Sync(); err != nil {
		return transcript.Stored{}, errors.Join(err, tmp.Close())
	}
	if err := tmp.Close(); err != nil {
		return transcript.Stored{}, err
	}
	// Two calls storing the same content both get here: the second rename
	// replaces a file with the same bytes.
	if err := os.Rename(tmp.Name(), final); err != nil {
		return transcript.Stored{}, err
	}
	return stored, s.syncDir(blobs)
}

// OpenBlob reads content stored by PutBlob. The content is hashed as it is
// read, and the read that would end it returns a *CorruptError instead of
// io.EOF when the hash is not the one asked for.
func (s *Store) OpenBlob(ctx context.Context, id uuid.UUID, hash string) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	name, err := blobName(hash)
	if err != nil {
		return nil, err
	}
	log, _, err := s.openLog(id, os.O_RDONLY, syscall.LOCK_SH)
	if err != nil {
		return nil, err
	}
	defer log.Close() //nolint:errcheck // Read only.
	f, err := os.Open(filepath.Join(s.SessionDir(id), blobsName, name))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("%w: session %s has no content %s", store.ErrNotFound, id, hash)
		}
		return nil, err
	}
	want, err := hex.DecodeString(name)
	if err != nil {
		return nil, errors.Join(err, f.Close())
	}
	return &blobReader{f: f, sum: sha256.New(), want: want}, nil
}

// blobReader checks the content against its hash as it is read.
type blobReader struct {
	f    *os.File
	sum  hash.Hash
	want []byte
	read int64
}

func (b *blobReader) Read(p []byte) (int, error) {
	n, err := b.f.Read(p)
	b.sum.Write(p[:n]) // A hash never fails to write.
	b.read += int64(n)
	if errors.Is(err, io.EOF) && !bytes.Equal(b.sum.Sum(nil), b.want) {
		return n, &CorruptError{Path: b.f.Name(), Offset: b.read, Err: errors.New("the content does not have the hash it is stored under")}
	}
	return n, err
}

func (b *blobReader) Close() error { return b.f.Close() }

// ctxReader stops a copy when its context ends.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}
