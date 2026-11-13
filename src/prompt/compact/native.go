package compact

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"uuid"

	"github.com/gfx-labs/towa/src/provider"
	"github.com/gfx-labs/towa/src/transcript"
)

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

// NativeItem builds the item of a native compaction. The provider's items
// ride in the item's Opaque, tagged with providerName, and Items gives them
// back to that provider only. The item has no summary text: the provider's
// summary is encrypted. The explicit endpoint's whole returned window is
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

// nativeWindow returns the provider's own items stored in a native
// compaction entry. It fails with UnreadableError when one of them is
// opaque to the target.
func nativeWindow(entry transcript.Entry, providerName string, target transcript.Origin) ([]transcript.Item, error) {
	item := entry.Item
	var native []transcript.Item
	if err := json.Unmarshal(item.Opaque, &native); err != nil {
		return nil, fmt.Errorf("compact: read the native items of compaction %s: %w", entry.ID, err)
	}
	if err := validateNativeWindow(native, providerName); err != nil {
		return nil, fmt.Errorf("compact: stored native window %s: %w", entry.ID, err)
	}
	for _, child := range native {
		if len(child.Opaque) > 0 && !child.OpaqueCompatible(providerName, target) {
			return nil, &UnreadableError{Entry: entry.ID, Provider: item.Provider, Wanted: providerName}
		}
	}
	return native, nil
}
