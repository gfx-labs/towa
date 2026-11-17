package cli

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/gfx-labs/towa/src/auth"
	"github.com/gfx-labs/towa/src/config"
	"github.com/gfx-labs/towa/src/provider"
	"github.com/gfx-labs/towa/src/provider/models"
)

// modelListsPath is the file the providers' model lists are kept in, beside
// the auth store's directory.
func modelListsPath(h Host) (string, error) {
	dir, err := auth.StateDir(lookup(h.Environ, "XDG_STATE_HOME"), lookup(h.Environ, "HOME"))
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(dir), "models.json"), nil
}

// unlistedModels returns where the figures of a model the built-in table
// does not have come from. For each such configured model it reads the
// account's model list from the provider, unless the kept one is younger
// than model_list_max_age_seconds. The list is read even when the
// configuration gives both figures, because it also says whether the model
// takes adaptive thinking.
//
// A list that cannot be read is logged and not an error: the model's name
// may still say its figures, and a model nothing is known about is refused
// when it is selected.
func (f *runtimeFactory) unlistedModels(ctx context.Context, now time.Time) (models.Unlisted, error) {
	cfg := f.cfg
	unlisted := models.Unlisted{
		Providers:  cfg.Providers,
		Configured: models.Metadata{ContextWindow: cfg.ModelContextWindow, MaxOutputTokens: cfg.ModelMaxOutputTokens},
		Lists:      models.Lists{},
		Accounts:   cfg.Accounts,
	}
	wanted, err := f.modelsWithLists()
	if err != nil {
		return models.Unlisted{}, err
	}
	if len(wanted) == 0 {
		return unlisted, nil
	}
	path, err := modelListsPath(f.host)
	if err != nil {
		return models.Unlisted{}, err
	}
	if unlisted.Lists, err = models.LoadLists(path); err != nil {
		return models.Unlisted{}, err
	}
	if f.refreshLists(ctx, unlisted.Lists, wanted, now) {
		if err := unlisted.Lists.Save(path); err != nil {
			return models.Unlisted{}, err
		}
	}
	return unlisted, nil
}

// modelsWithLists returns the configured models whose provider can list
// its models. A model the table has is among them: the table gives no
// reasoning efforts.
func (f *runtimeFactory) modelsWithLists() ([]config.ChainEntry, error) {
	var wanted []config.ChainEntry
	for _, raw := range f.cfg.Models {
		entry, err := config.ParseChainEntry(raw)
		if err != nil {
			return nil, fmt.Errorf("models: %w", err)
		}
		if _, lists := f.providers.clients[entry.Provider].(provider.ModelLister); lists {
			wanted = append(wanted, config.ExpandAccounts(entry, f.cfg.Accounts)...)
		}
	}
	return wanted, nil
}

// refreshLists reads into lists the model list of each wanted entry whose
// kept list is too old, and reports whether it changed lists.
func (f *runtimeFactory) refreshLists(ctx context.Context, lists models.Lists, wanted []config.ChainEntry, now time.Time) (changed bool) {
	maxAge := time.Duration(f.cfg.ModelListMaxAgeSeconds) * time.Second
	timeout := time.Duration(f.cfg.ModelListTimeoutSeconds) * time.Second
	for _, entry := range wanted {
		key := models.ListKey(entry)
		if kept, ok := lists[key]; ok && now.Sub(time.UnixMilli(kept.FetchedAtUnixMs)) < maxAge {
			continue
		}
		list, err := fetchModelList(ctx, f.providers, entry, timeout)
		if errors.Is(err, provider.ErrNoModelList) {
			continue
		}
		if err != nil {
			f.tel.Logger.Warn("the provider's model list was not read", "provider", entry.Provider, "account", entry.Account, "model", entry.Model, "err", err)
			continue
		}
		list.FetchedAtUnixMs = now.UnixMilli()
		lists[key], changed = list, true
	}
	return changed
}

func fetchModelList(ctx context.Context, p *Providers, entry config.ChainEntry, timeout time.Duration) (models.List, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cred, err := p.Credentials.Credential(ctx, entry.Account)
	if err != nil {
		return models.List{}, err
	}
	infos, err := p.clients[entry.Provider].(provider.ModelLister).Models(ctx, cred)
	if err != nil {
		return models.List{}, err
	}
	list := models.List{Models: make(map[string]models.Listed, len(infos))}
	for _, info := range infos {
		list.Models[info.ID] = models.Listed{ContextWindow: info.ContextWindow, MaxOutputTokens: info.MaxOutputTokens, NoAdaptiveThinking: info.NoAdaptiveThinking, Efforts: info.Efforts}
	}
	return list, nil
}
