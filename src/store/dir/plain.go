package dir

import (
	"bufio"
	"bytes"
	"encoding/json/v2"
	"errors"
	"io"
	"iter"
	"os"

	"github.com/valyala/bytebufferpool"

	"github.com/gfx-labs/acpharness/src/store"
	"github.com/gfx-labs/acpharness/src/transcript"
)

// plainFormat is JSON lines. A record is the lines of the entries of one
// Append followed by a commit line:
//
//	{"id":...}
//	{"id":...}
//	{"commit":{"entries":2,"bytes":1234}}
//
// An Append is stored when its commit line is, so lines after the last
// commit line are a torn record, even when each of them is whole.
type plainFormat struct{}

// commitPrefix starts a commit line and no entry line: an entry's first
// member is its id.
var commitPrefix = []byte(`{"commit":`)

// commit closes a record.
type commit struct {
	// Entries is the number of entry lines in the record, and Bytes is
	// their length with their newlines.
	Entries int   `json:"entries"`
	Bytes   int64 `json:"bytes"`
}

type commitLine struct {
	Commit commit `json:"commit"`
}

func isCommit(line []byte) bool { return bytes.HasPrefix(line, commitPrefix) }

func (plainFormat) fileName() string { return logName }

func (plainFormat) end(f *os.File, size int64) (int64, error) {
	if size == 0 {
		return 0, nil
	}
	// An intact log ends in a commit line, which costs one short read.
	var last [1]byte
	if _, err := f.ReadAt(last[:], size-1); err != nil {
		return 0, err
	}
	whole := size
	if last[0] == '\n' {
		intact := false
		err := linesBack(f, size, func(offset int64, line []byte) (bool, error) {
			if !isCommit(line) {
				return true, nil
			}
			var c commitLine
			if err := json.Unmarshal(line, &c); err != nil {
				return false, &CorruptError{Path: f.Name(), Offset: offset, Err: err}
			}
			if c.Commit.Bytes <= 0 || c.Commit.Bytes > offset {
				return false, &CorruptError{Path: f.Name(), Offset: offset, Err: errors.New("the commit line counts bytes the log does not have")}
			}
			intact = true
			return true, nil
		})
		if err != nil || intact {
			return size, err
		}
	} else {
		// Drop the line that was cut short.
		buf := make([]byte, readChunk)
		for whole > 0 {
			n := min(int64(len(buf)), whole)
			if _, err := f.ReadAt(buf[:n], whole-n); err != nil {
				return 0, err
			}
			if i := bytes.LastIndexByte(buf[:n], '\n'); i >= 0 {
				whole = whole - n + int64(i) + 1
				break
			}
			whole -= n
		}
	}
	// Torn: the last whole record ends at the last commit line.
	end := int64(0)
	err := linesBack(f, whole, func(offset int64, line []byte) (bool, error) {
		if !isCommit(line) {
			return false, nil
		}
		end = offset + int64(len(line)) + 1
		return true, nil
	})
	return end, err
}

func (plainFormat) frame(dst *bytebufferpool.ByteBuffer, lines []byte, count int, _ bool) error {
	dst.B = append(dst.B, lines...)
	if err := json.MarshalWrite(dst, commitLine{Commit: commit{Entries: count, Bytes: int64(len(lines))}}); err != nil {
		return err
	}
	return dst.WriteByte('\n')
}

func (plainFormat) lastLine(f *os.File, end int64) ([]byte, error) {
	var last []byte
	err := linesBack(f, end, func(_ int64, line []byte) (bool, error) {
		if isCommit(line) {
			return false, nil
		}
		last = bytes.Clone(line)
		return true, nil
	})
	return last, err
}

func (p plainFormat) context(f *os.File, end int64) iter.Seq2[transcript.Entry, error] {
	return func(yield func(transcript.Entry, error) bool) {
		// The latest compaction is found by reading back from the end, so
		// what is before it is never read.
		var from int64
		err := linesBack(f, end, func(offset int64, line []byte) (bool, error) {
			if isCommit(line) {
				return false, nil
			}
			found, err := isCompaction(line)
			if err != nil {
				return false, &CorruptError{Offset: offset, Err: err}
			}
			from = offset
			return found, nil
		})
		if err != nil {
			yield(transcript.Entry{}, err)
			return
		}
		// Without a compaction the scan stopped at the first line.
		p.forward(f, from, end, store.Range{})(yield)
	}
}

func (p plainFormat) history(f *os.File, end int64, r store.Range) iter.Seq2[transcript.Entry, error] {
	return p.forward(f, 0, end, r)
}

// forward yields the entries at the positions in r, counting from the line
// at offset from.
func (plainFormat) forward(f *os.File, from, end int64, r store.Range) iter.Seq2[transcript.Entry, error] {
	return func(yield func(transcript.Entry, error) bool) {
		lines := lineReader{r: bufio.NewReaderSize(io.NewSectionReader(f, from, end-from), readChunk), offset: from}
		for position := 0; r.To == 0 || position < r.To; {
			offset := lines.offset
			line, err := lines.next()
			if errors.Is(err, io.EOF) {
				return
			}
			if err != nil {
				yield(transcript.Entry{}, err)
				return
			}
			if isCommit(line) {
				continue
			}
			position++
			if position <= r.From {
				continue
			}
			e, err := decodeLine(line)
			if err != nil {
				yield(transcript.Entry{}, &CorruptError{Offset: offset, Err: err})
				return
			}
			if !yield(e, nil) {
				return
			}
		}
	}
}

// lineReader reads lines of any length. A line is valid until the next call.
type lineReader struct {
	r      *bufio.Reader
	long   []byte
	offset int64
}

func (l *lineReader) next() ([]byte, error) {
	line, err := l.r.ReadSlice('\n')
	if errors.Is(err, bufio.ErrBufferFull) {
		l.long = append(l.long[:0], line...)
		for errors.Is(err, bufio.ErrBufferFull) {
			line, err = l.r.ReadSlice('\n')
			l.long = append(l.long, line...)
		}
		line = l.long
	}
	if err != nil {
		// The section ends in a newline, so the end comes with no bytes.
		return nil, err
	}
	l.offset += int64(len(line))
	return line[:len(line)-1], nil
}

// linesBack calls visit with each line before end and its offset, the last
// line first, until visit returns true. The log before end ends in a
// newline. A line is valid until visit returns.
func linesBack(f io.ReaderAt, end int64, visit func(offset int64, line []byte) (bool, error)) error {
	buf := make([]byte, readChunk)
	// tail holds the end of the line being assembled, in pieces from the
	// last to the first. They are kept apart until the line's first byte is
	// found, so that a long line is copied once.
	var tail [][]byte
	var line []byte
	emit := func(offset int64, head []byte) (bool, error) {
		if len(tail) == 0 {
			return visit(offset, head[:len(head)-1])
		}
		line = append(line[:0], head...)
		for i := len(tail) - 1; i >= 0; i-- {
			line = append(line, tail[i]...)
		}
		tail = tail[:0]
		return visit(offset, line[:len(line)-1])
	}
	for pos := end; pos > 0; {
		n := min(int64(len(buf)), pos)
		pos -= n
		chunk := buf[:n]
		if _, err := f.ReadAt(chunk, pos); err != nil {
			return err
		}
		// hi is the end of the line being looked for. The newline that ends
		// it is the last byte of the chunk on the first pass, or is in tail.
		hi := len(chunk)
		for {
			search := hi
			if len(tail) == 0 {
				search = hi - 1
			}
			i := bytes.LastIndexByte(chunk[:search], '\n')
			if i < 0 {
				break
			}
			stop, err := emit(pos+int64(i)+1, chunk[i+1:hi])
			if err != nil || stop {
				return err
			}
			hi = i + 1
		}
		if pos == 0 {
			_, err := emit(0, chunk[:hi])
			return err
		}
		tail = append(tail, bytes.Clone(chunk[:hi]))
	}
	return nil
}
