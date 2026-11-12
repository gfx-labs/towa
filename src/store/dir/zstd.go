package dir

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"iter"
	"os"
	"sync"

	"github.com/klauspost/compress/zstd"
	"github.com/valyala/bytebufferpool"

	"github.com/gfx-labs/towa/src/store"
	"github.com/gfx-labs/towa/src/transcript"
)

// A record of the compressed log is the JSON lines of one Append as one
// zstd frame, between two copies of a 24 byte marker:
//
//	marker | zstd frame | marker
//
// The marker is a zstd skippable frame, so the file is a zstd stream that
// `zstd -dc log.jsonl.zst` turns back into the JSON lines. It holds the
// frame's length, so the log can be walked from either end without
// decompressing, and the number of entries and whether one of them is a
// compaction, so a range or the latest compaction is found the same way.
const (
	// skippableMagic is the first of the sixteen magic numbers RFC 8878
	// section 3.1.2 reserves for skippable frames.
	skippableMagic = 0x184D2A50
	markerPayload  = 16
	markerSize     = 8 + markerPayload

	flagCompaction = 1
)

const zstdLogName = "log.jsonl.zst"

type marker struct {
	frameLen uint64
	count    uint32
	flags    uint32
}

func (m marker) appendTo(dst []byte) []byte {
	dst = binary.LittleEndian.AppendUint32(dst, skippableMagic)
	dst = binary.LittleEndian.AppendUint32(dst, markerPayload)
	dst = binary.LittleEndian.AppendUint64(dst, m.frameLen)
	dst = binary.LittleEndian.AppendUint32(dst, m.count)
	return binary.LittleEndian.AppendUint32(dst, m.flags)
}

// readMarker reads the marker at offset. It reports false for bytes that
// are not one.
func readMarker(f *os.File, offset int64) (marker, bool, error) {
	var b [markerSize]byte
	if _, err := f.ReadAt(b[:], offset); err != nil {
		return marker{}, false, err
	}
	if binary.LittleEndian.Uint32(b[0:]) != skippableMagic || binary.LittleEndian.Uint32(b[4:]) != markerPayload {
		return marker{}, false, nil
	}
	return marker{
		frameLen: binary.LittleEndian.Uint64(b[8:]),
		count:    binary.LittleEndian.Uint32(b[16:]),
		flags:    binary.LittleEndian.Uint32(b[20:]),
	}, true, nil
}

func (m marker) recordLen() int64 { return 2*markerSize + int64(m.frameLen) }

// codec holds the zstd encoder and decoder, made when first used so that a
// store with no compressed session does not hold their memory.
type codec struct {
	encOnce, decOnce sync.Once
	enc              *zstd.Encoder
	dec              *zstd.Decoder
	encErr, decErr   error
}

func (c *codec) encoder() (*zstd.Encoder, error) {
	c.encOnce.Do(func() {
		// One goroutine: EncodeAll runs on the caller's. The level is the
		// library's default.
		c.enc, c.encErr = zstd.NewWriter(nil, zstd.WithEncoderConcurrency(1))
	})
	return c.enc, c.encErr
}

func (c *codec) decoder() (*zstd.Decoder, error) {
	c.decOnce.Do(func() {
		c.dec, c.decErr = zstd.NewReader(nil, zstd.WithDecoderConcurrency(1))
	})
	return c.dec, c.decErr
}

// zstdFormat is the compressed log.
type zstdFormat struct {
	codec *codec
}

func (zstdFormat) fileName() string { return zstdLogName }

func (zstdFormat) end(f *os.File, size int64) (int64, error) {
	// An intact log ends in a marker whose record starts with the same one.
	if size == 0 {
		return 0, nil
	}
	if size >= 2*markerSize {
		last, ok, err := readMarker(f, size-markerSize)
		if err != nil {
			return 0, err
		}
		if ok && last.recordLen() <= size {
			first, ok, err := readMarker(f, size-last.recordLen())
			if err != nil {
				return 0, err
			}
			if ok && first == last {
				return size, nil
			}
		}
	}
	// Torn: walk the records from the start to find where the last whole
	// one ends.
	pos := int64(0)
	for size-pos >= 2*markerSize {
		first, ok, err := readMarker(f, pos)
		if err != nil {
			return 0, err
		}
		if !ok || first.recordLen() > size-pos {
			break
		}
		last, ok, err := readMarker(f, pos+first.recordLen()-markerSize)
		if err != nil {
			return 0, err
		}
		if !ok || first != last {
			break
		}
		pos += first.recordLen()
	}
	return pos, nil
}

func (z zstdFormat) frame(dst *bytebufferpool.ByteBuffer, lines []byte, count int, compaction bool) error {
	enc, err := z.codec.encoder()
	if err != nil {
		return err
	}
	m := marker{count: uint32(count)}
	if int(m.count) != count {
		return fmt.Errorf("store/dir: %d entries in one append do not fit the record's count", count)
	}
	if compaction {
		m.flags = flagCompaction
	}
	// The frame's length is known after it is encoded: leave room for the
	// first marker and fill it in.
	start := len(dst.B)
	dst.B = append(dst.B, make([]byte, markerSize)...)
	dst.B = enc.EncodeAll(lines, dst.B)
	m.frameLen = uint64(len(dst.B) - start - markerSize)
	m.appendTo(dst.B[start:start])
	dst.B = m.appendTo(dst.B)
	return nil
}

// record is one record's place in the log.
type record struct {
	offset int64
	marker
}

// lines decompresses a record into buf and returns its JSON lines.
func (z zstdFormat) lines(f *os.File, rec record, compressed, buf *bytebufferpool.ByteBuffer) ([]byte, error) {
	dec, err := z.codec.decoder()
	if err != nil {
		return nil, err
	}
	if uint64(cap(compressed.B)) < rec.frameLen {
		compressed.B = make([]byte, rec.frameLen)
	}
	compressed.B = compressed.B[:rec.frameLen]
	if _, err := f.ReadAt(compressed.B, rec.offset+markerSize); err != nil {
		return nil, err
	}
	buf.B, err = dec.DecodeAll(compressed.B, buf.B[:0])
	if err != nil {
		return nil, &CorruptError{Offset: rec.offset, Err: err}
	}
	return buf.B, nil
}

// back returns the record that ends at end.
func (zstdFormat) back(f *os.File, end int64) (record, error) {
	m, ok, err := readMarker(f, end-markerSize)
	if err != nil {
		return record{}, err
	}
	if !ok || m.recordLen() > end {
		return record{}, &CorruptError{Offset: end - markerSize, Err: errors.New("not a record marker")}
	}
	return record{offset: end - m.recordLen(), marker: m}, nil
}

func (z zstdFormat) lastLine(f *os.File, end int64) ([]byte, error) {
	if end == 0 {
		return nil, nil
	}
	rec, err := z.back(f, end)
	if err != nil {
		return nil, err
	}
	compressed, buf := bytebufferpool.Get(), bytebufferpool.Get()
	defer bytebufferpool.Put(compressed)
	defer bytebufferpool.Put(buf)
	lines, err := z.lines(f, rec, compressed, buf)
	if err != nil {
		return nil, err
	}
	lines = bytes.TrimSuffix(lines, []byte{'\n'})
	return bytes.Clone(lines[bytes.LastIndexByte(lines, '\n')+1:]), nil
}

func (z zstdFormat) context(f *os.File, end int64) iter.Seq2[transcript.Entry, error] {
	return func(yield func(transcript.Entry, error) bool) {
		// Walk the markers back to the latest record with a compaction.
		from, compacted := end, false
		for from > 0 && !compacted {
			rec, err := z.back(f, from)
			if err != nil {
				yield(transcript.Entry{}, err)
				return
			}
			from, compacted = rec.offset, rec.flags&flagCompaction != 0
		}
		z.forward(f, from, end, store.Range{}, compacted)(yield)
	}
}

func (z zstdFormat) history(f *os.File, end int64, r store.Range) iter.Seq2[transcript.Entry, error] {
	return z.forward(f, 0, end, r, false)
}

// forward yields the entries at the positions in r, counting from the
// record at offset from. With fromCompaction it starts at the last
// compaction entry of that record.
func (z zstdFormat) forward(f *os.File, from, end int64, r store.Range, fromCompaction bool) iter.Seq2[transcript.Entry, error] {
	return func(yield func(transcript.Entry, error) bool) {
		compressed, buf := bytebufferpool.Get(), bytebufferpool.Get()
		defer bytebufferpool.Put(compressed)
		defer bytebufferpool.Put(buf)
		position := 0
		for pos := from; pos < end && (r.To == 0 || position < r.To); {
			m, ok, err := readMarker(f, pos)
			if err == nil && (!ok || m.recordLen() > end-pos) {
				err = &CorruptError{Offset: pos, Err: errors.New("not a record marker")}
			}
			if err != nil {
				yield(transcript.Entry{}, err)
				return
			}
			rec := record{offset: pos, marker: m}
			pos += m.recordLen()
			if position+int(m.count) <= r.From {
				// Nothing of this record is in the range.
				position += int(m.count)
				continue
			}
			lines, err := z.lines(f, rec, compressed, buf)
			if err != nil {
				yield(transcript.Entry{}, err)
				return
			}
			if fromCompaction {
				fromCompaction = false
				if lines, err = fromLastCompaction(lines); err != nil {
					yield(transcript.Entry{}, &CorruptError{Offset: rec.offset, Err: err})
					return
				}
			}
			for len(lines) > 0 && (r.To == 0 || position < r.To) {
				line, rest, found := bytes.Cut(lines, []byte{'\n'})
				if !found {
					yield(transcript.Entry{}, &CorruptError{Offset: rec.offset, Err: errors.New("the record does not end in a newline")})
					return
				}
				lines = rest
				position++
				if position <= r.From {
					continue
				}
				e, err := decodeLine(line)
				if err != nil {
					yield(transcript.Entry{}, &CorruptError{Offset: rec.offset, Err: err})
					return
				}
				if !yield(e, nil) {
					return
				}
			}
		}
	}
}

// fromLastCompaction returns lines from its last compaction entry on.
func fromLastCompaction(lines []byte) ([]byte, error) {
	for rest := lines; len(rest) > 0; {
		body := rest[:len(rest)-1]
		start := bytes.LastIndexByte(body, '\n') + 1
		found, err := isCompaction(body[start:])
		if err != nil {
			return nil, err
		}
		if found {
			return lines[start:], nil
		}
		rest = rest[:start]
	}
	return nil, errors.New("the record is marked as holding a compaction and has none")
}
