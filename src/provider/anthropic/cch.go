package anthropic

import (
	"encoding/binary"
	"math/bits"
	"strings"
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
// returns the body with the placeholder replaced.
func signBilling(body []byte) string {
	signed := string(body)
	hash := xxh64([]byte(cchPreimage(signed)), cchSeed)
	return strings.Replace(signed, cchPlaceholder, "cch="+hexToken(hash), 1)
}

// cchPreimage is the body as Claude Code hashes it: the model value is
// emptied and the max_tokens field is removed.
func cchPreimage(body string) string {
	body = stripModelValue(body)
	return stripMaxTokens(body)
}

// stripModelValue empties the model value. The request builder writes the
// model field first, so its value is the first string in the body.
func stripModelValue(body string) string {
	const field = `"model":"`
	n := strings.Index(body, field)
	if n < 0 {
		return body
	}
	start := n + len(field)
	end := start
	for end < len(body) && body[end] != '"' {
		end++
	}
	return body[:start] + body[end:]
}

// stripMaxTokens removes the max_tokens field with its separator.
func stripMaxTokens(body string) string {
	const field = `"max_tokens":`
	n := strings.Index(body, field)
	if n < 0 {
		return body
	}
	end := n + len(field)
	for end < len(body) && !strings.ContainsRune(",}", rune(body[end])) {
		end++
	}
	if end < len(body) && body[end] == ',' {
		end++
	}
	return body[:n] + body[end:]
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

// xxh64 is xxHash64 with a seed, the 64-bit variant without a secret.
func xxh64(data []byte, seed uint64) uint64 {
	n := uint64(len(data))
	var h uint64
	if len(data) >= 32 {
		v1 := seed + xxhPrime1 + xxhPrime2
		v2 := seed + xxhPrime2
		v3 := seed
		v4 := seed - xxhPrime1
		for len(data) >= 32 {
			v1 = xxhRound(v1, binary.LittleEndian.Uint64(data[0:]))
			v2 = xxhRound(v2, binary.LittleEndian.Uint64(data[8:]))
			v3 = xxhRound(v3, binary.LittleEndian.Uint64(data[16:]))
			v4 = xxhRound(v4, binary.LittleEndian.Uint64(data[24:]))
			data = data[32:]
		}
		h = bits.RotateLeft64(v1, 1) + bits.RotateLeft64(v2, 7) +
			bits.RotateLeft64(v3, 12) + bits.RotateLeft64(v4, 18)
		h = xxhMerge(h, v1)
		h = xxhMerge(h, v2)
		h = xxhMerge(h, v3)
		h = xxhMerge(h, v4)
	} else {
		h = seed + xxhPrime5
	}
	h += n
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
