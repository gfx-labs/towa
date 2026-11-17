package models

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/gfx-labs/towa/src/config"
)

// Listed is what a provider's model list says about one model. A zero
// figure is one the list does not give.
type Listed struct {
	ContextWindow   int64 `json:"context_window,omitzero"`
	MaxOutputTokens int64 `json:"max_output_tokens,omitzero"`
	// NoAdaptiveThinking says the list marks adaptive thinking as not
	// supported.
	NoAdaptiveThinking bool `json:"no_adaptive_thinking,omitzero"`
	// Efforts are the reasoning efforts the list says the model takes.
	Efforts []string `json:"efforts,omitzero"`
}

// List is one account's model list, as its provider gave it.
type List struct {
	// FetchedAtUnixMs is when the list was read from the provider.
	FetchedAtUnixMs int64 `json:"fetched_at_unix_ms"`
	// Models are the listed models by the provider's own id.
	Models map[string]Listed `json:"models"`
}

// Lists are the model lists read from providers, by "provider:account". A
// list belongs to an account because a provider lists what that account can
// use. They are kept in a JSON file so that a start does not ask again for
// what it already has.
type Lists map[string]List

// ListKey is the key of an entry's account in Lists.
func ListKey(e config.ChainEntry) string { return e.Provider + ":" + e.Account }

// Lookup returns what the entry's account's list says about the entry's
// model.
func (l Lists) Lookup(e config.ChainEntry) (Listed, bool) {
	list, ok := l[ListKey(e)]
	if !ok {
		return Listed{}, false
	}
	return list.find(e.Model)
}

// AdaptiveThinking reports whether a model of the provider takes the
// adaptive thinking parameter. The table says so for a model it has. For
// another, a list of one of the provider's accounts that marks it as not
// supported says no, and so does the row its name points to, so
// claude-haiku-4-5-20251001 is refused it as claude-haiku-4-5 is. A model
// nothing speaks for is taken to: the API refuses the request by name if it
// does not.
func (l Lists) AdaptiveThinking(providerID, model string) bool {
	if meta, ok := builtin[model]; ok {
		return !meta.NoAdaptiveThinking
	}
	for key, list := range l {
		if m, ok := list.find(model); ok && m.NoAdaptiveThinking && strings.HasPrefix(key, providerID+":") {
			return false
		}
	}
	guessed, _, _ := guess(model)
	return !guessed.NoAdaptiveThinking
}

// find returns what the list says about a model. A list names a model with
// its release date ("claude-opus-4-5-20251101"), so a name without one
// matches it.
func (list List) find(model string) (Listed, bool) {
	if m, ok := list.Models[model]; ok {
		return m, true
	}
	want, ok := parseName(model)
	if !ok {
		return Listed{}, false
	}
	var found []string
	for id := range list.Models {
		if have, ok := parseName(id); ok && have == want {
			found = append(found, id)
		}
	}
	// Two dated releases of one model may differ, and nothing says which
	// the plain name means.
	if len(found) != 1 {
		return Listed{}, false
	}
	return list.Models[found[0]], true
}

// LoadLists reads the lists file. A file that does not exist holds none.
func LoadLists(path string) (Lists, error) {
	data, err := os.ReadFile(path) //nolint:gosec // the path is the harness's own state file
	if errors.Is(err, fs.ErrNotExist) {
		return Lists{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("models: %w", err)
	}
	lists := Lists{}
	if err := json.Unmarshal(data, &lists); err != nil {
		return nil, fmt.Errorf("models: %s: %w", path, err)
	}
	return lists, nil
}

// Save replaces the lists file, by a temp file in the same directory and a
// rename, so that a reader never sees half of it.
func (l Lists) Save(path string) (err error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("models: %w", err)
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("models: %w", err)
	}
	defer func() {
		if err != nil {
			err = errors.Join(fmt.Errorf("models: %w", err), os.Remove(tmp.Name()))
		}
	}()
	// Sorted and on several lines: the file is one a person may read.
	if err := json.MarshalWrite(tmp, l, json.Deterministic(true), jsontext.Multiline(true)); err != nil {
		return errors.Join(err, tmp.Close())
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
