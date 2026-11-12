package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"time"

	"github.com/gfx-labs/towa/src/auth"
	"github.com/gfx-labs/towa/src/config"
	"github.com/gfx-labs/towa/src/models"
	"github.com/gfx-labs/towa/src/provider"
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
func unlistedModels(ctx context.Context, cfg config.Config, h Host, p *Providers, logger *slog.Logger, now time.Time) (models.Unlisted, error) {
	unlisted := models.Unlisted{
		Configured: models.Metadata{ContextWindow: cfg.ModelContextWindow, MaxOutputTokens: cfg.ModelMaxOutputTokens},
		Lists:      models.Lists{},
	}
	var wanted []config.ChainEntry
	for _, raw := range cfg.Models {
		entry, err := config.ParseChainEntry(raw)
		if err != nil {
			return models.Unlisted{}, fmt.Errorf("models: %w", err)
		}
		if _, lists := p.clients[entry.Provider].(provider.ModelLister); lists && !models.InTable(entry.Model) {
			wanted = append(wanted, entry)
		}
	}
	if len(wanted) == 0 {
		return unlisted, nil
	}
	path, err := modelListsPath(h)
	if err != nil {
		return models.Unlisted{}, err
	}
	if unlisted.Lists, err = models.LoadLists(path); err != nil {
		return models.Unlisted{}, err
	}
	maxAge := time.Duration(cfg.ModelListMaxAgeSeconds) * time.Second
	timeout := time.Duration(cfg.ModelListTimeoutSeconds) * time.Second
	changed := false
	for _, entry := range wanted {
		key := models.ListKey(entry)
		if kept, ok := unlisted.Lists[key]; ok && now.Sub(time.UnixMilli(kept.FetchedAtUnixMs)) < maxAge {
			continue
		}
		list, err := fetchModelList(ctx, p, entry, timeout)
		if errors.Is(err, provider.ErrNoModelList) {
			continue
		}
		if err != nil {
			logger.Warn("the provider's model list was not read", "provider", entry.Provider, "account", entry.Account, "model", entry.Model, "err", err)
			continue
		}
		list.FetchedAtUnixMs = now.UnixMilli()
		unlisted.Lists[key], changed = list, true
	}
	if changed {
		if err := unlisted.Lists.Save(path); err != nil {
			return models.Unlisted{}, err
		}
	}
	return unlisted, nil
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
		list.Models[info.ID] = models.Listed{ContextWindow: info.ContextWindow, MaxOutputTokens: info.MaxOutputTokens, NoAdaptiveThinking: info.NoAdaptiveThinking}
	}
	return list, nil
}
