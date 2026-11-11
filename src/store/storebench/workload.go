package storebench

import (
	"encoding/base64"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io/fs"
	"math/rand/v2"
	"strings"
	"time"
	"unicode/utf8"
	"uuid"

	"github.com/gfx-labs/acpharness/src/transcript"
)

// The sizes of the parts of a generated turn that Workload does not set.
// They are in the range seen in coding sessions: a short prompt, a paragraph
// of reasoning with a signature only its provider reads, a few paragraphs of
// answer. The signature is random bytes, as a real one is to a compressor.
const (
	userBytes      = 200
	thinkingBytes  = 400
	signatureBytes = 256
	answerBytes    = 600
	summaryBytes   = 2000
)

// Workload describes the session a benchmark writes. The text comes from
// real files, because the size on disk and the scan time depend on how well
// the text compresses and a made-up text would decide that by accident.
type Workload struct {
	// Corpus holds the text files the turns are cut from. Files that are
	// not valid UTF-8 are left out.
	Corpus fs.FS
	// Seed makes the session the same on every run.
	Seed uint64
	// Turns is the number of prompt turns.
	Turns int
	// ToolCalls is the number of tool calls in each turn.
	ToolCalls int
	// ResultBytes is the size of each tool result.
	ResultBytes int
	// CompactEvery puts a compaction entry at the start of every turn whose
	// number is a multiple of it. Zero means the session is never compacted.
	CompactEvery int
	// AppendPerEntry appends each entry on its own. Otherwise a turn is one
	// append.
	AppendPerEntry bool
	// Keyword is what the scan looks for in the full history.
	Keyword string
}

func (w Workload) validate() error {
	switch {
	case w.Corpus == nil:
		return errors.New("storebench: the workload has no corpus")
	case w.Turns <= 0:
		return fmt.Errorf("storebench: turns: %d is not positive", w.Turns)
	case w.ToolCalls < 0:
		return fmt.Errorf("storebench: tool calls: %d is negative", w.ToolCalls)
	case w.ToolCalls > 0 && w.ResultBytes <= 0:
		return fmt.Errorf("storebench: result bytes: %d is not positive", w.ResultBytes)
	case w.CompactEvery < 0:
		return fmt.Errorf("storebench: compact every: %d is negative", w.CompactEvery)
	}
	return nil
}

// Generator produces the turns of a workload.
type Generator struct {
	w     Workload
	text  string
	rng   *rand.Rand
	now   time.Time
	last  uuid.UUID
	turns int
}

// NewGenerator reads the corpus and returns a generator at the first turn.
func NewGenerator(w Workload) (*Generator, error) {
	if err := w.validate(); err != nil {
		return nil, err
	}
	var text strings.Builder
	err := fs.WalkDir(w.Corpus, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		data, err := fs.ReadFile(w.Corpus, path)
		if err != nil {
			return err
		}
		if utf8.Valid(data) {
			text.Write(data)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("storebench: read the corpus: %w", err)
	}
	// A chunk may start up to three bytes late to land on a rune.
	longest := max(w.ResultBytes, summaryBytes) + utf8.UTFMax
	if text.Len() <= longest {
		return nil, fmt.Errorf("storebench: the corpus has %d bytes of text and a turn needs more than %d", text.Len(), longest)
	}
	return &Generator{
		w:    w,
		text: text.String(),
		rng:  rand.New(rand.NewPCG(w.Seed, w.Seed)),
		// Any fixed time: the entries only need times that differ.
		now: time.Date(2026, time.November, 11, 0, 0, 0, 0, time.UTC),
	}, nil
}

// chunk returns n bytes of the corpus that start and end on a rune.
func (g *Generator) chunk(n int) string {
	start := g.rng.IntN(len(g.text) - n - utf8.UTFMax)
	for !utf8.RuneStart(g.text[start]) {
		start++
	}
	end := start + n
	for !utf8.RuneStart(g.text[end]) {
		end--
	}
	return g.text[start:end]
}

func (g *Generator) entry(item transcript.Item) transcript.Entry {
	g.now = g.now.Add(time.Second)
	e := transcript.NewEntry(g.last, g.now, item)
	g.last = e.ID
	return e
}

// Turn returns the entries of the next turn: the prompt, then for each tool
// call the reasoning, the call and its result, then the answer and the usage.
func (g *Generator) Turn() ([]transcript.Entry, error) {
	g.turns++
	entries := make([]transcript.Entry, 0, 4+3*g.w.ToolCalls)
	if g.w.CompactEvery > 0 && g.turns%g.w.CompactEvery == 0 {
		entries = append(entries, g.entry(transcript.Item{Compaction: &transcript.Compaction{
			Summary: g.chunk(summaryBytes), FirstKept: g.last, Mode: "summary",
		}}))
	}
	entries = append(entries, g.entry(transcript.Item{User: &transcript.User{
		Parts: []transcript.Part{{Text: g.chunk(userBytes)}},
	}}))
	for range g.w.ToolCalls {
		signature := make([]byte, signatureBytes)
		for i := range signature {
			signature[i] = byte(g.rng.Uint32())
		}
		opaque, err := json.Marshal(map[string]string{"signature": base64.StdEncoding.EncodeToString(signature)})
		if err != nil {
			return nil, err
		}
		input, err := json.Marshal(map[string]string{"path": g.chunk(40)})
		if err != nil {
			return nil, err
		}
		callID := fmt.Sprintf("call_%016x", g.rng.Uint64())
		entries = append(entries,
			g.entry(transcript.Item{
				Thinking: &transcript.Thinking{Text: g.chunk(thinkingBytes)},
				Provider: "anthropic", Opaque: jsontext.Value(opaque),
			}),
			g.entry(transcript.Item{ToolCall: &transcript.ToolCall{ID: callID, Name: "read", Input: jsontext.Value(input)}}),
			g.entry(transcript.Item{ToolResult: &transcript.ToolResult{
				CallID: callID, Name: "read", Outcome: transcript.OutcomeCompleted,
				Parts: []transcript.Part{{Text: g.chunk(g.w.ResultBytes)}},
			}}),
		)
	}
	entries = append(entries,
		g.entry(transcript.Item{Text: &transcript.Text{Text: g.chunk(answerBytes)}}),
		g.entry(transcript.Item{Usage: &transcript.Usage{
			Model: "bench", InputTokens: g.rng.Int64N(200_000), OutputTokens: g.rng.Int64N(8_000),
			CacheReadTokens: g.rng.Int64N(200_000),
		}}),
	)
	return entries, nil
}
