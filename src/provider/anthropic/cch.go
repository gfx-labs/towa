package anthropic

import (
	"bytes"
	"encoding/binary"
	"math/bits"
)

// The Claude Code subscription billing hash: Anthropic folds the request
// body into a 20-bit token in the billing block's cch field. The values below
// come from Claude Code 2.1.325's binary, where the seeded xxHash64 and the
// preimage transform reproduce every live request recorded here.
//
// The token is not what the API checks. An OAuth request is refused with 429
// without a billing block and accepted with any cch in it, so this reproduces
// Claude Code's token rather than a token that has to be right.
const (
	// cchSeed is SEED_2_1_138 of Claude Code's cch, unchanged since 2.1.138.
	cchSeed uint64 = 0x4D659218E32A3268
	// cchPlaceholder is what the hash is computed over, in place of the
	// token. Claude Code serializes it before signing.
	cchPlaceholder = "cch=00000"
)

const (
	xxhPrime1 uint64 = 0x9E3779B185EBCA87
	xxhPrime2 uint64 = 0xC2B2AE3D27D4EB4F
	xxhPrime3 uint64 = 0x165667B19E3779F9
	xxhPrime4 uint64 = 0x85EBCA77C2B2AE63
	xxhPrime5 uint64 = 0x27D4EB2F165667C5
)

// signBilling computes the cch token of a serialized request body and
// writes it over the placeholder in place. The token has the placeholder's
// length, so nothing moves and the body is not copied: a long session's
// request is hundreds of kilobytes, signed on every turn.
func signBilling(body []byte) {
	var d xxhDigest
	d.reset(cchSeed)
	for _, part := range cchPreimage(body) {
		d.write(part)
	}
	at := bytes.Index(body, []byte(cchPlaceholder))
	if at < 0 {
		return
	}
	copy(body[at+len("cch="):], hexToken(d.sum()))
}

// cchPreimage is the body as Claude Code hashes it, given as the pieces
// that remain when the model value is emptied and the max_tokens field is
// removed. The pieces are slices of body.
func cchPreimage(body []byte) [][]byte {
	type cut struct{ start, end int }
	var cuts []cut
	// The request builder writes the model field first, so its value is
	// the first string in the body.
	if n := bytes.Index(body, []byte(`"model":"`)); n >= 0 {
		start := n + len(`"model":"`)
		end := start
		for end < len(body) && body[end] != '"' {
			end++
		}
		cuts = append(cuts, cut{start, end})
	}
	// The model value holds no quote, so emptying it does not change where
	// this field is found.
	if n := bytes.Index(body, []byte(`"max_tokens":`)); n >= 0 {
		end := n + len(`"max_tokens":`)
		for end < len(body) && body[end] != ',' && body[end] != '}' {
			end++
		}
		if end < len(body) && body[end] == ',' {
			end++
		}
		cuts = append(cuts, cut{n, end})
	}
	if len(cuts) == 2 && cuts[1].start < cuts[0].start {
		cuts[0], cuts[1] = cuts[1], cuts[0]
	}
	pieces := make([][]byte, 0, len(cuts)+1)
	from := 0
	for _, c := range cuts {
		pieces = append(pieces, body[from:c.start])
		from = c.end
	}
	return append(pieces, body[from:])
}

func hexToken(hash uint64) string {
	const digits = "0123456789abcdef"
	var out [5]byte
	for i := range out {
		out[len(out)-1-i] = digits[(hash>>(4*uint(i)))&0xf]
	}
	return string(out[:])
}

func xxhRound(acc, lane uint64) uint64 {
	acc += lane * xxhPrime2
	acc = bits.RotateLeft64(acc, 31)
	return acc * xxhPrime1
}

func xxhMerge(acc, lane uint64) uint64 {
	acc ^= xxhRound(0, lane)
	return acc*xxhPrime1 + xxhPrime4
}

func xxhAvalanche(h uint64) uint64 {
	h ^= h >> 33
	h *= xxhPrime2
	h ^= h >> 29
	h *= xxhPrime3
	h ^= h >> 32
	return h
}

// xxhDigest is xxHash64 with a seed, the 64-bit variant without a secret,
// fed in pieces.
type xxhDigest struct {
	v      [4]uint64
	total  uint64
	buf    [32]byte
	buffed int
	seed   uint64
}

func (d *xxhDigest) reset(seed uint64) {
	*d = xxhDigest{seed: seed}
	d.v = [4]uint64{seed + xxhPrime1 + xxhPrime2, seed + xxhPrime2, seed, seed - xxhPrime1}
}

func (d *xxhDigest) stripes(data []byte) []byte {
	for len(data) >= 32 {
		for i := range d.v {
			d.v[i] = xxhRound(d.v[i], binary.LittleEndian.Uint64(data[8*i:]))
		}
		data = data[32:]
	}
	return data
}

func (d *xxhDigest) write(data []byte) {
	d.total += uint64(len(data))
	if d.buffed > 0 {
		n := copy(d.buf[d.buffed:], data)
		d.buffed += n
		data = data[n:]
		if d.buffed < 32 {
			return
		}
		d.stripes(d.buf[:])
		d.buffed = 0
	}
	d.buffed = copy(d.buf[:], d.stripes(data))
}

func (d *xxhDigest) sum() uint64 {
	var h uint64
	if d.total >= 32 {
		h = bits.RotateLeft64(d.v[0], 1) + bits.RotateLeft64(d.v[1], 7) +
			bits.RotateLeft64(d.v[2], 12) + bits.RotateLeft64(d.v[3], 18)
		for _, v := range d.v {
			h = xxhMerge(h, v)
		}
	} else {
		h = d.seed + xxhPrime5
	}
	h += d.total
	data := d.buf[:d.buffed]
	for len(data) >= 8 {
		h ^= xxhRound(0, binary.LittleEndian.Uint64(data))
		h = bits.RotateLeft64(h, 27)*xxhPrime1 + xxhPrime4
		data = data[8:]
	}
	if len(data) >= 4 {
		h ^= uint64(binary.LittleEndian.Uint32(data)) * xxhPrime1
		h = bits.RotateLeft64(h, 23)*xxhPrime2 + xxhPrime3
		data = data[4:]
	}
	for _, b := range data {
		h ^= uint64(b) * xxhPrime5
		h = bits.RotateLeft64(h, 11) * xxhPrime1
	}
	return xxhAvalanche(h)
}
