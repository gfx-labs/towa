package compact

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/gfx-labs/towa/src/config"
	"github.com/gfx-labs/towa/src/transcript"
)

// Embedder turns texts into vectors, one per input and in the same order.
// The client in src/embed is one.
type Embedder interface {
	Embed(ctx context.Context, inputs []string) ([][]float32, error)
}

// Cosine is the cosine similarity of two vectors. It is zero when either
// has no length or they differ in size.
func Cosine(a, b []float32) float64 {
	if len(a) != len(b) {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// mean is the element-wise mean of vectors of one size.
func mean(vectors [][]float32) []float32 {
	out := make([]float32, len(vectors[0]))
	for _, v := range vectors {
		if len(v) != len(out) {
			return nil
		}
		for i, x := range v {
			out[i] += x
		}
	}
	for i := range out {
		out[i] /= float32(len(vectors))
	}
	return out
}

// TopicShift reports whether the conversation moved to another topic: the
// mean of the older half of the turn embeddings and the mean of the newer
// half are less similar than threshold. With fewer than four embeddings
// there is nothing to compare.
func TopicShift(embeddings [][]float32, threshold float64) bool {
	if len(embeddings) < 4 {
		return false
	}
	half := len(embeddings) / 2
	older, newer := mean(embeddings[:half]), mean(embeddings[half:])
	if older == nil || newer == nil {
		return false
	}
	return Cosine(older, newer) < threshold
}

// messageText is the text of one message for embedding: what the user and
// the model wrote, without tool calls, results or thinking, cut to maxChars
// characters.
func messageText(entries []transcript.Entry, maxChars int) string {
	var b strings.Builder
	for _, e := range entries {
		switch {
		case e.Item.User != nil:
			for _, p := range e.Item.User.Parts {
				if p.Stored != nil {
					continue
				}
				b.WriteString(p.Text)
				b.WriteByte('\n')
			}
		case e.Item.Text != nil:
			b.WriteString(e.Item.Text.Text)
			b.WriteByte('\n')
		}
	}
	text := strings.TrimSpace(b.String())
	if runes := []rune(text); len(runes) > maxChars {
		text = string(runes[:maxChars])
	}
	return text
}

// SemanticKeep returns how many of the latest messages a semantic
// compaction of active keeps. It is at least keep_recent_messages. It is
// more when an older message is relevant to the current goal: the goal is
// the text of the latest goal_window_messages, and every message from the
// earliest one whose similarity to it reaches relevance_keep_threshold is
// kept, because the replaced part has to be one unbroken run.
//
// It sends one request to the embedder. A failure is returned, never treated
// as permission to switch semantic mode to proactive.
func SemanticKeep(ctx context.Context, embedder Embedder, cfg config.Compaction, active []transcript.Entry) (int, error) {
	if embedder == nil {
		return 0, errors.New("compact: semantic mode needs an embedder")
	}
	msgs := spans(active)
	candidates := len(msgs) - cfg.KeepRecentMessages
	if candidates <= 0 {
		return cfg.KeepRecentMessages, nil
	}
	goalFrom := max(len(msgs)-cfg.GoalWindowMessages, 0)
	goal := messageText(active[msgs[goalFrom].start:], cfg.EmbedMaxChars)
	if goal == "" {
		return cfg.KeepRecentMessages, nil
	}
	// A message with no text of its own, such as a run of tool results, is
	// not scored.
	inputs, index := []string{goal}, []int{-1}
	for i, m := range msgs[:candidates] {
		if text := messageText(active[m.start:m.end], cfg.EmbedMaxChars); text != "" {
			inputs, index = append(inputs, text), append(index, i)
		}
	}
	vectors, err := embedder.Embed(ctx, inputs)
	if err != nil {
		return 0, fmt.Errorf("compact: embed messages: %w", err)
	}
	if len(vectors) != len(inputs) {
		return 0, fmt.Errorf("compact: the embedder returned %d vectors for %d inputs", len(vectors), len(inputs))
	}
	if err := validateVectors(vectors); err != nil {
		return 0, err
	}
	for i := 1; i < len(vectors); i++ {
		if Cosine(vectors[0], vectors[i]) < cfg.RelevanceKeepThreshold {
			continue
		}
		// jcode keeps the plain count when fewer than two messages would
		// be replaced.
		if index[i] < 2 {
			break
		}
		return len(msgs) - index[i], nil
	}
	return cfg.KeepRecentMessages, nil
}

// EmbedTurn embeds the plain user/assistant text of a completed turn for
// topic-shift detection. The loop passes only that turn's entries. No text
// means no vector and no request. src/embed.Client implements Embedder.
func EmbedTurn(ctx context.Context, embedder Embedder, cfg config.Compaction, entries []transcript.Entry) ([]float32, error) {
	if embedder == nil {
		return nil, errors.New("compact: semantic mode needs an embedder")
	}
	text := messageText(entries, cfg.EmbedMaxChars)
	if text == "" {
		return nil, nil
	}
	vectors, err := embedder.Embed(ctx, []string{text})
	if err != nil {
		return nil, fmt.Errorf("compact: embed turn: %w", err)
	}
	if len(vectors) != 1 {
		return nil, fmt.Errorf("compact: embed turn returned %d vectors for one input", len(vectors))
	}
	if err := validateVectors(vectors); err != nil {
		return nil, err
	}
	return vectors[0], nil
}

func validateVectors(vectors [][]float32) error {
	if len(vectors) == 0 {
		return errors.New("compact: the embedder returned no vectors")
	}
	for i, vector := range vectors {
		if len(vector) == 0 || len(vector) != len(vectors[0]) {
			return fmt.Errorf("compact: embedding %d has an empty or different dimension", i)
		}
		var norm float64
		for _, x := range vector {
			if math.IsNaN(float64(x)) || math.IsInf(float64(x), 0) {
				return fmt.Errorf("compact: embedding %d has a non-finite value", i)
			}
			norm += float64(x) * float64(x)
		}
		if norm == 0 {
			return fmt.Errorf("compact: embedding %d has zero norm", i)
		}
	}
	return nil
}
