package dir

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io/fs"
	"iter"
	"os"
	"path/filepath"
	"strconv"
	"time"
	"uuid"

	"github.com/valyala/bytebufferpool"

	"github.com/gfx-labs/acpharness/src/filelock"
	"github.com/gfx-labs/acpharness/src/store"
	"github.com/gfx-labs/acpharness/src/transcript"
)

// readChunk is the size of the buffer a log is read through. It is not a
// limit: a longer line is assembled from several reads.
const readChunk = 64 << 10

// TornError reports a log that ends in a record that was not written whole,
// which a crash during Append leaves behind. The entries before Offset are
// intact. Nothing is appended to the session until Repair has set the torn
// bytes aside. It matches store.ErrTorn.
type TornError struct {
	Path string
	// Offset is where the torn record starts, and Size is the log's size.
	Offset, Size int64
}

func (e *TornError) Error() string {
	return fmt.Sprintf("store/dir: %s ends in a torn record: %d bytes from offset %d", e.Path, e.Size-e.Offset, e.Offset)
}

// Is makes errors.Is(err, store.ErrTorn) true.
func (e *TornError) Is(target error) bool { return target == store.ErrTorn }

// logFormat is how entries are laid out in a log file. In every format an
// Append is one record, so that a crash leaves all of its entries or none.
type logFormat interface {
	fileName() string
	// end returns where the last whole record of a log of the given size
	// ends. It is less than size when the log has a torn tail.
	end(f *os.File, size int64) (int64, error)
	// frame appends to dst the record that holds lines, the JSON lines of
	// count entries.
	frame(dst *bytebufferpool.ByteBuffer, lines []byte, count int, compaction bool) error
	// lastLine returns the JSON of the last entry before end, or nil for an
	// empty log.
	lastLine(f *os.File, end int64) ([]byte, error)
	// context yields the latest compaction entry before end and every
	// entry after it, or every entry when there is no compaction.
	context(f *os.File, end int64) iter.Seq2[transcript.Entry, error]
	// history yields the entries before end at the positions in r.
	history(f *os.File, end int64, r store.Range) iter.Seq2[transcript.Entry, error]
}

// encodeLines appends the entries to dst as JSON lines and reports whether
// one of them is a compaction.
func encodeLines(dst *bytebufferpool.ByteBuffer, entries []transcript.Entry) (compaction bool, err error) {
	for i, e := range entries {
		if e.ID == uuid.Nil() {
			return false, fmt.Errorf("store/dir: entry %d has no id", i)
		}
		if err := e.Item.Validate(); err != nil {
			return false, fmt.Errorf("store/dir: entry %d (%s): %w", i, e.ID, err)
		}
		// The encoder writes no newline inside a value, and compacts raw
		// JSON that has one, so a newline always ends an entry.
		if err := json.MarshalWrite(dst, e); err != nil {
			return false, fmt.Errorf("store/dir: entry %d (%s): %w", i, e.ID, err)
		}
		_ = dst.WriteByte('\n') // Never fails.
		compaction = compaction || e.Item.Compaction != nil
	}
	return compaction, nil
}

// decodeLine decodes one entry.
func decodeLine(line []byte) (transcript.Entry, error) {
	var e transcript.Entry
	if err := json.Unmarshal(line, &e); err != nil {
		return transcript.Entry{}, err
	}
	if err := e.Item.Validate(); err != nil {
		return transcript.Entry{}, err
	}
	return e, nil
}

// compactionKey is in every line that holds a compaction entry. It can also
// be in a line that holds none, inside a tool's input or a provider's
// opaque value, so a line that has it is decoded to find out.
var compactionKey = []byte(`"compaction":`)

func isCompaction(line []byte) (bool, error) {
	if !bytes.Contains(line, compactionKey) {
		return false, nil
	}
	var e struct {
		Item struct {
			Compaction jsontext.Value `json:"compaction"`
		} `json:"item"`
	}
	if err := json.Unmarshal(line, &e); err != nil {
		return false, err
	}
	return e.Item.Compaction != nil, nil
}

// checkRange reports a range that selects nothing by mistake. Positions
// past the end of the history are not a mistake: they select no entry.
func checkRange(r store.Range) error {
	if r.From < 0 || r.To < 0 || (r.To != 0 && r.To < r.From) {
		return fmt.Errorf("store/dir: range from %d to %d is not valid", r.From, r.To)
	}
	return nil
}

// Append adds entries to a session's log with one write and one fsync.
func (s *Store) Append(ctx context.Context, id uuid.UUID, entries []transcript.Entry) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	lines := bytebufferpool.Get()
	defer bytebufferpool.Put(lines)
	compaction, err := encodeLines(lines, entries)
	if err != nil {
		return err
	}
	f, format, err := s.openLog(id, os.O_RDWR, filelock.Exclusive)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		return f.Close()
	}
	if err := s.append(f, format, lines.B, len(entries), compaction); err != nil {
		return errors.Join(err, f.Close())
	}
	return f.Close()
}

func (s *Store) append(f *os.File, format logFormat, lines []byte, count int, compaction bool) error {
	info, err := f.Stat()
	if err != nil {
		return err
	}
	size := info.Size()
	end, err := format.end(f, size)
	if err != nil {
		return err
	}
	if end != size {
		return &TornError{Path: f.Name(), Offset: end, Size: size}
	}
	out := bytebufferpool.Get()
	defer bytebufferpool.Put(out)
	if err := format.frame(out, lines, count, compaction); err != nil {
		return err
	}
	// One write, so that a crash tears at most the record being added.
	_, err = f.WriteAt(out.B, size)
	if err == nil {
		s.syncs.Add(1)
		err = f.Sync()
	}
	if err != nil {
		// The entries were not stored. Take back what was written, so that
		// the failure does not leave a torn record for the next Append.
		rollback := f.Truncate(size)
		if rollback == nil {
			s.syncs.Add(1)
			rollback = f.Sync()
		}
		return errors.Join(err, rollback)
	}
	return nil
}

// view is what a reader needs of a session: the log is read up to end, and
// the bytes below it never change.
type view struct {
	meta      store.Meta
	path      string
	format    logFormat
	size, end int64
}

// snapshot reads a session's Meta and where its log ends, under the lock.
func (s *Store) snapshot(id uuid.UUID) (view, error) {
	f, format, err := s.openLog(id, os.O_RDONLY, filelock.Shared)
	if err != nil {
		return view{}, err
	}
	defer f.Close() //nolint:errcheck // Read only: closing drops the lock and cannot lose data.
	v := view{path: f.Name(), format: format}
	if v.meta, err = s.readMeta(id); err != nil {
		return view{}, err
	}
	info, err := f.Stat()
	if err != nil {
		return view{}, err
	}
	v.size = info.Size()
	if v.end, err = format.end(f, v.size); err != nil {
		return view{}, err
	}
	// Append does not rewrite meta.json, which would cost three more fsyncs
	// a turn: the session was updated when its last entry was made.
	line, err := format.lastLine(f, v.end)
	if err != nil {
		return view{}, err
	}
	if line != nil {
		var last struct {
			Time time.Time `json:"time"`
		}
		if err := json.Unmarshal(line, &last); err != nil {
			return view{}, &CorruptError{Path: v.path, Offset: v.end, Err: err}
		}
		v.meta.Updated = last.Time
	}
	return v, nil
}

// read opens the log when the sequence is ranged over, so that a sequence
// nobody reads holds no file. A torn tail is reported after the last whole
// entry, so the caller has both the entries and the fault.
func (v view) read(ctx context.Context, entries func(f *os.File) iter.Seq2[transcript.Entry, error]) iter.Seq2[transcript.Entry, error] {
	return func(yield func(transcript.Entry, error) bool) {
		f, err := os.Open(v.path)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				err = fmt.Errorf("%w: %s", store.ErrNotFound, v.meta.ID)
			}
			yield(transcript.Entry{}, err)
			return
		}
		defer f.Close() //nolint:errcheck // Read only.
		for e, err := range entries(f) {
			if err == nil {
				err = ctx.Err()
			}
			if err != nil {
				var corrupt *CorruptError
				if errors.As(err, &corrupt) && corrupt.Path == "" {
					corrupt.Path = v.path
				}
				yield(transcript.Entry{}, err)
				return
			}
			if !yield(e, nil) {
				return
			}
		}
		if v.end != v.size {
			yield(transcript.Entry{}, &TornError{Path: v.path, Offset: v.end, Size: v.size})
		}
	}
}

// Load returns the session's Meta and the entries in its context. When the
// log has a torn tail, the sequence ends with a *TornError after the last
// whole entry.
func (s *Store) Load(ctx context.Context, id uuid.UUID) (store.Meta, iter.Seq2[transcript.Entry, error], error) {
	if err := ctx.Err(); err != nil {
		return store.Meta{}, nil, err
	}
	v, err := s.snapshot(id)
	if err != nil {
		return store.Meta{}, nil, err
	}
	return v.meta, v.read(ctx, func(f *os.File) iter.Seq2[transcript.Entry, error] {
		return v.format.context(f, v.end)
	}), nil
}

// History returns the entries at the positions in r. A position past the
// end of the history selects nothing. A torn tail is reported as in Load,
// when the range reaches the end.
func (s *Store) History(ctx context.Context, id uuid.UUID, r store.Range) (iter.Seq2[transcript.Entry, error], error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := checkRange(r); err != nil {
		return nil, err
	}
	v, err := s.snapshot(id)
	if err != nil {
		return nil, err
	}
	if v.end == v.size || r.To == 0 {
		return v.read(ctx, func(f *os.File) iter.Seq2[transcript.Entry, error] {
			return v.format.history(f, v.end, r)
		}), nil
	}
	to := r.To
	// Read until the requested bound is satisfied or the torn tail is
	// reached. A bound beyond the intact entries cannot hide the fault.
	r.To = 0
	seq := v.read(ctx, func(f *os.File) iter.Seq2[transcript.Entry, error] {
		return v.format.history(f, v.end, r)
	})
	return func(yield func(transcript.Entry, error) bool) {
		position := r.From
		for e, err := range seq {
			if err != nil {
				yield(e, err)
				return
			}
			if to != 0 && position >= to {
				return
			}
			position++
			if !yield(e, nil) || (to != 0 && position == to) {
				return
			}
		}
	}, nil
}

// Repair sets the torn tail of a session's log aside, in a file named
// torn-<offset> in the session's directory, and cuts the log back to its
// last whole record. It returns the number of bytes set aside, which is
// zero for a log that is not torn. The file is never removed by the store.
func (s *Store) Repair(ctx context.Context, id uuid.UUID) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	f, format, err := s.openLog(id, os.O_RDWR, filelock.Exclusive)
	if err != nil {
		return 0, err
	}
	n, err := s.repair(f, format, s.SessionDir(id))
	return n, errors.Join(err, f.Close())
}

func (s *Store) repair(f *os.File, format logFormat, dir string) (int64, error) {
	info, err := f.Stat()
	if err != nil {
		return 0, err
	}
	end, err := format.end(f, info.Size())
	if err != nil || end == info.Size() {
		return 0, err
	}
	torn := make([]byte, info.Size()-end)
	if _, err := f.ReadAt(torn, end); err != nil {
		return 0, err
	}
	// A second crash can tear another append at the same offset. A unique
	// name keeps the bytes from every repair, rather than replacing one.
	aside, err := os.CreateTemp(dir, "torn-"+strconv.FormatInt(end, 10)+"-*")
	if err != nil {
		return 0, err
	}
	err = errors.Join(s.writeSync(aside, torn), aside.Close())
	if err != nil {
		return 0, err
	}
	if err := s.syncDir(dir); err != nil {
		return 0, err
	}
	if err := f.Truncate(end); err != nil {
		return 0, err
	}
	s.syncs.Add(1)
	return int64(len(torn)), f.Sync()
}

// logPath is where a format keeps a session's log.
func logPath(dir string, format logFormat) string {
	return filepath.Join(dir, format.fileName())
}
