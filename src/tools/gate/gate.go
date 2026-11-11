// Package gate keeps a large tool result out of the model's context until
// the model asks for it.
package gate

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"strings"

	"github.com/gfx-labs/acpharness/pkg/acp"
	"github.com/gfx-labs/acpharness/src/config"
	"github.com/gfx-labs/acpharness/src/tools"
	"github.com/gfx-labs/acpharness/src/transcript"
)

// AcceptMember is the member of a tool's argument that asks for a result
// whatever its size. The design fixes the name for every dialect.
const AcceptMember = "accept_large_output"

// ErrAcceptNotBoolean reports an AcceptMember that is not true or false.
var ErrAcceptNotBoolean = errors.New("gate: " + AcceptMember + " is not a boolean")

// Split takes AcceptMember out of a tool's argument and returns the rest
// and the member's value. An argument that is not an object, or has no such
// member, is returned as it is. A dialect whose schemas declare the member
// runs repair first, so that "true" sent as a string is repaired there.
func Split(input jsontext.Value) (rest jsontext.Value, accept bool, err error) {
	if input.Kind() != '{' || !bytes.Contains(input, []byte(AcceptMember)) {
		return input, false, nil
	}
	dec := jsontext.NewDecoder(bytes.NewReader(input))
	var out bytes.Buffer
	enc := jsontext.NewEncoder(&out)
	begin, err := dec.ReadToken()
	if err != nil {
		return nil, false, fmt.Errorf("gate: %w", err)
	}
	if err := enc.WriteToken(begin); err != nil {
		return nil, false, fmt.Errorf("gate: %w", err)
	}
	found := false
	for dec.PeekKind() != '}' {
		name, err := dec.ReadValue()
		if err != nil {
			return nil, false, fmt.Errorf("gate: %w", err)
		}
		// The name is still raw here, so it keeps its quotes.
		isAccept := string(name) == `"`+AcceptMember+`"`
		name = name.Clone()
		value, err := dec.ReadValue()
		if err != nil {
			return nil, false, fmt.Errorf("gate: %w", err)
		}
		if isAccept {
			switch value.Kind() {
			case 't':
				accept = true
			case 'f':
			default:
				return nil, false, ErrAcceptNotBoolean
			}
			found = true
			continue
		}
		if err := errors.Join(enc.WriteValue(name), enc.WriteValue(value)); err != nil {
			return nil, false, fmt.Errorf("gate: %w", err)
		}
	}
	if !found {
		return input, false, nil
	}
	if err := enc.WriteToken(jsontext.EndObject); err != nil {
		return nil, false, fmt.Errorf("gate: %w", err)
	}
	return bytes.TrimSuffix(out.Bytes(), []byte("\n")), accept, nil
}

// Summary describes a result that was withheld.
type Summary struct {
	// EstimatedTokens is the size of the result's text, estimated from its
	// bytes. BudgetTokens is what it was over.
	EstimatedTokens int64
	BudgetTokens    int64
	// Bytes and Lines are the size of the text that was withheld.
	Bytes int64
	Lines int
	// Stored is where the full text was kept. It is nil when the session
	// has no store or the store failed.
	Stored *transcript.Stored
	// StoreErr is why the full text could not be kept. The result is gated
	// all the same.
	StoreErr error
}

// Text is the summary in plain words, for a dialect with none of its own.
func (s Summary) Text() string {
	return fmt.Sprintf(
		"The result is about %d tokens (%d bytes, %d lines), over the limit of %d tokens, and was withheld. "+
			"Narrow the call, or run it again with %s set to true to get the full result.",
		s.EstimatedTokens, s.Bytes, s.Lines, s.BudgetTokens, AcceptMember,
	)
}

// Estimate returns the estimated tokens of a result's text. Images and
// audio are not counted: their cost in tokens is not a function of their
// size in bytes, and each provider prices them its own way.
func Estimate(cfg config.ToolsGate, parts []transcript.Part) int64 {
	n := textBytes(parts)
	return (n + cfg.BytesPerToken - 1) / cfg.BytesPerToken
}

func textBytes(parts []transcript.Part) int64 {
	var n int64
	for _, part := range parts {
		n += int64(len(text(part)))
	}
	return n
}

func text(part transcript.Part) string {
	if part.Resource != nil {
		return part.Resource.Text
	}
	return part.Text
}

// Apply returns res as it is when gating is off, the call accepted a large
// result, or the result is within the budget. Otherwise the text parts are
// replaced by one summary and, when the session has a store, a reference to
// the full text kept there. Parts that are not text stay. What the client
// is shown does not change: when res has no Content of its own, the full
// text becomes its Content.
func Apply(ctx context.Context, env tools.Env, cfg config.ToolsGate, res tools.Result, accept bool) (tools.Result, *Summary) {
	if !cfg.Enabled || accept {
		return res, nil
	}
	estimated := Estimate(cfg, res.Parts)
	if estimated <= cfg.BudgetTokens {
		return res, nil
	}
	var full strings.Builder
	var kept []transcript.Part
	for _, part := range res.Parts {
		if t := text(part); t != "" {
			full.WriteString(t)
			continue
		}
		kept = append(kept, part)
	}
	sum := &Summary{
		EstimatedTokens: estimated,
		BudgetTokens:    cfg.BudgetTokens,
		Bytes:           int64(full.Len()),
		Lines:           lines(full.String()),
	}
	if env.PutBlob != nil {
		ref, err := env.PutBlob(ctx, "text/plain; charset=utf-8", strings.NewReader(full.String()))
		if err != nil {
			sum.StoreErr = err
		} else {
			sum.Stored = &ref
		}
	}
	out := tools.Result{Outcome: res.Outcome, Content: res.Content}
	if len(out.Content) == 0 {
		// The client was going to be shown Parts. It still gets the text.
		out.Content = []acp.ToolCallContent{tools.TextContent(full.String())}
	}
	out.Parts = append(out.Parts, transcript.Part{Text: sum.Text()})
	if sum.Stored != nil {
		out.Parts = append(out.Parts, transcript.Part{Stored: sum.Stored})
	}
	out.Parts = append(out.Parts, kept...)
	return out, sum
}

// lines counts lines as wc does, plus a last line with no newline.
func lines(s string) int {
	n := strings.Count(s, "\n")
	if s != "" && !strings.HasSuffix(s, "\n") {
		n++
	}
	return n
}
