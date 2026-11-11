// Package compact decides when a session's context is compacted, builds the
// request that asks a model for the summary, and reads a transcript that
// holds compactions.
//
// A compaction is one appended entry. It names the first entry that is
// still sent and holds the summary that stands for everything before it.
// Nothing here removes or rewrites an entry: the replaced entries stay in
// the store, where conversation search reads them.
package compact

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"
	"time"
	"uuid"

	"github.com/gfx-labs/acpharness/src/provider"
	"github.com/gfx-labs/acpharness/src/transcript"
)

// Values of transcript.Compaction.Mode that are not a configured mode.
const (
	// ModeManual: the user asked for the compaction.
	ModeManual = "manual"
	// ModeEmergency: the context was cut without a model summary, because
	// the next request would not have fit.
	ModeEmergency = "emergency"
	// ModeNative: the provider compacted the conversation itself.
	ModeNative = "native"
)

// ErrEmptySummary reports a compaction with no summary and no native items.
var ErrEmptySummary = errors.New("compact: the summary is empty")

// UnreadableError reports a native compaction that the provider about to be
// asked cannot read, and that has no summary text. The entries it replaced
// are still in the store, so the caller can summarise them again.
type UnreadableError struct {
	// Entry is the compaction entry.
	Entry uuid.UUID
	// Provider produced the compaction. Wanted is the one being asked.
	Provider, Wanted string
}

func (e *UnreadableError) Error() string {
	return fmt.Sprintf("compact: compaction %s was made by provider %q and has no summary that %q can read", e.Entry, e.Provider, e.Wanted)
}

// View is a transcript as the next request sees it.
type View struct {
	// Compaction is the latest compaction entry, or nil when there is none.
	Compaction *transcript.Entry
	// Active holds the entries still sent, in order: the ones from the
	// compaction's first kept entry on, without compaction entries.
	Active []transcript.Entry
}

// Summary is the text of the latest compaction, or empty.
func (v View) Summary() string {
	if v.Compaction == nil {
		return ""
	}
	return v.Compaction.Item.Compaction.Summary
}

// Read finds the latest compaction in entries and the entries it kept. It
// returns slices of entries and changes nothing.
func Read(entries []transcript.Entry) (View, error) {
	last := -1
	for i := range entries {
		if entries[i].Item.Compaction != nil {
			last = i
		}
	}
	if last < 0 {
		return View{Active: entries}, nil
	}
	c := entries[last].Item.Compaction
	// A nil id means nothing before the compaction was kept.
	first := last
	if c.FirstKept != uuid.Nil() {
		first = -1
		for i := range entries[:last] {
			if entries[i].ID == c.FirstKept {
				first = i
				break
			}
		}
		if first < 0 {
			return View{}, fmt.Errorf("compact: compaction %s keeps from entry %s, which is not before it in the transcript",
				entries[last].ID, c.FirstKept)
		}
	}
	active := make([]transcript.Entry, 0, len(entries)-first)
	for _, e := range entries[first:] {
		if e.Item.Compaction == nil {
			active = append(active, e)
		}
	}
	return View{Compaction: &entries[last], Active: active}, nil
}

// Items returns the items to send to providerName after any compaction: the
// summary as a user message, or the provider's own items for a native
// compaction it made, and then the kept entries' items. Without a
// compaction it returns every entry's item.
//
// The result still holds Usage, ModelSwitch and Interrupted items. The
// loop's Conversation deals with those, as it does without a compaction.
func Items(entries []transcript.Entry, providerName string, target ...transcript.Origin) ([]transcript.Item, error) {
	view, err := Read(entries)
	if err != nil {
		return nil, err
	}
	items := make([]transcript.Item, 0, len(view.Active)+1)
	if view.Compaction != nil {
		head, err := standIn(*view.Compaction, providerName, target...)
		if err != nil {
			return nil, err
		}
		items = append(items, head...)
	}
	for _, e := range view.Active {
		items = append(items, e.Item)
	}
	return items, nil
}

// standIn returns the items that stand for what a compaction replaced.
func standIn(entry transcript.Entry, providerName string, target ...transcript.Origin) ([]transcript.Item, error) {
	item := entry.Item
	if len(target) > 1 {
		return nil, errors.New("compact: more than one target origin")
	}
	if len(target) == 1 && item.OpaqueCompatible(providerName, target[0]) {
		var native []transcript.Item
		if err := json.Unmarshal(item.Opaque, &native); err != nil {
			return nil, fmt.Errorf("compact: read the native items of compaction %s: %w", entry.ID, err)
		}
		if err := validateNativeWindow(native, providerName); err != nil {
			return nil, fmt.Errorf("compact: stored native window %s: %w", entry.ID, err)
		}
		for _, child := range native {
			if len(child.Opaque) > 0 && !child.OpaqueCompatible(providerName, target[0]) {
				return nil, &UnreadableError{Entry: entry.ID, Provider: item.Provider, Wanted: providerName}
			}
		}
		return native, nil
	}
	summary := item.Compaction.Summary
	if strings.TrimSpace(summary) == "" {
		if len(item.Opaque) > 0 {
			return nil, &UnreadableError{Entry: entry.ID, Provider: item.Provider, Wanted: providerName}
		}
		return nil, fmt.Errorf("compaction %s: %w", entry.ID, ErrEmptySummary)
	}
	text := summaryPrefix + summary + summarySuffix
	return []transcript.Item{{User: &transcript.User{Parts: []transcript.Part{{Text: text}}}}}, nil
}

// Entry returns the entry to append for a finished compaction. firstKept is
// the id of the first entry that is still sent, from Cut. mode is the
// configured mode that started it, or ModeManual or ModeEmergency.
func Entry(parent uuid.UUID, now time.Time, summary string, firstKept uuid.UUID, mode string) (transcript.Entry, error) {
	if strings.TrimSpace(summary) == "" {
		return transcript.Entry{}, ErrEmptySummary
	}
	if mode == "" {
		return transcript.Entry{}, errors.New("compact: the compaction has no mode")
	}
	return transcript.NewEntry(parent, now, transcript.Item{
		Compaction: &transcript.Compaction{Summary: summary, FirstKept: firstKept, Mode: mode},
	}), nil
}

// Native asks a provider to compact a conversation itself. The OpenAI
// Responses client's Compact does this. Its result is that package's own
// type, so the wiring wraps it in a NativeFunc.
type Native interface {
	Compact(ctx context.Context, req provider.Request) (NativeResult, error)
}

// NativeFunc is a function that is a Native.
type NativeFunc func(ctx context.Context, req provider.Request) (NativeResult, error)

// Compact calls f.
func (f NativeFunc) Compact(ctx context.Context, req provider.Request) (NativeResult, error) {
	return f(ctx, req)
}

// NativeResult is what a provider returned for a native compaction.
type NativeResult struct {
	// Items stand for the request's items in later requests to the same
	// provider. No other provider can read them.
	Items []transcript.Item
	// Usage is nil when the provider reported none.
	Usage *transcript.Usage
}

// NativeEntry returns the entry to append for a native compaction. The
// provider's items ride in the entry's Opaque, tagged with providerName, and
// Items gives them back to that provider only. The entry has no summary
// text: the provider's summary is encrypted.
func NativeEntry(parent uuid.UUID, now time.Time, result NativeResult, providerName string, firstKept uuid.UUID) (transcript.Entry, error) {
	item, err := NativeItem(result, providerName, firstKept)
	if err != nil {
		return transcript.Entry{}, err
	}
	return transcript.NewEntry(parent, now, item), nil
}

// NativeItem builds the item without choosing the caller's entry id,
// parent or timestamp. The explicit endpoint's whole returned window is
// retained, including any user or tool items beside the encrypted item.
func NativeItem(result NativeResult, providerName string, firstKept uuid.UUID) (transcript.Item, error) {
	if len(result.Items) == 0 {
		return transcript.Item{}, ErrEmptySummary
	}
	if providerName == "" {
		return transcript.Item{}, errors.New("compact: the native compaction names no provider")
	}
	if err := validateNativeWindow(result.Items, providerName); err != nil {
		return transcript.Item{}, err
	}
	opaque, err := json.Marshal(result.Items)
	if err != nil {
		return transcript.Item{}, fmt.Errorf("compact: encode the native items: %w", err)
	}
	var origin *transcript.Origin
	for _, item := range result.Items {
		if item.Native != nil && item.Native.Kind == "compaction" && item.Origin != nil {
			value := *item.Origin
			origin = &value
			break
		}
	}
	return transcript.Item{
		Compaction: &transcript.Compaction{FirstKept: firstKept, Mode: ModeNative},
		Provider:   providerName,
		Opaque:     opaque,
		Origin:     origin,
	}, nil
}

func validateNativeWindow(items []transcript.Item, providerName string) error {
	found := false
	for i, item := range items {
		if err := item.Validate(); err != nil {
			return fmt.Errorf("compact: native item %d: %w", i, err)
		}
		if item.Compaction != nil || item.Usage != nil || item.ModelSwitch != nil || item.Interrupted != nil {
			return fmt.Errorf("compact: native item %d is not a provider request item", i)
		}
		if item.Native != nil {
			if item.Provider != providerName || len(item.Opaque) == 0 {
				return fmt.Errorf("compact: native item %d has a different provider or no content", i)
			}
			found = found || item.Native.Kind == "compaction"
		}
	}
	if !found {
		return errors.New("compact: native window contains no compaction item")
	}
	return nil
}

// AutoItem converts a completed server-side compaction item at its stream
// position. Append it instead of the Native item. Everything before it is
// represented by it, and entries appended afterward remain active. This is
// for stateless input-array chaining, never previous_response_id chaining.
// Non-compaction items are returned unchanged with converted false.
func AutoItem(item transcript.Item) (out transcript.Item, converted bool, err error) {
	if item.Native == nil || item.Native.Kind != "compaction" {
		return item, false, nil
	}
	if len(item.Opaque) == 0 {
		return transcript.Item{}, false, errors.New("compact: automatic compaction has no opaque content")
	}
	out, err = NativeItem(NativeResult{Items: []transcript.Item{item}}, item.Provider, uuid.Nil())
	return out, err == nil, err
}

// EmergencySummary is the summary recorded when the context is cut without
// asking a model. It is the earlier summary, if any, and jcode's sentence
// saying how much was cut, with the names of the tools the cut entries
// used. contextTokens and budget are in tokens.
func EmergencySummary(previous string, cut []transcript.Entry, contextTokens, budget int64) string {
	var parts []string
	if strings.TrimSpace(previous) != "" {
		parts = append(parts, previous)
	}
	parts = append(parts, fmt.Sprintf(emergencyFormat, len(spans(cut)), contextTokens/1000, budget/1000))
	if names := toolNames(cut); len(names) > 0 {
		parts = append(parts, "Tools used: "+strings.Join(names, ", "))
	}
	return strings.Join(parts, "\n\n")
}
