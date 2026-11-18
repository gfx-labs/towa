package cli

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/fsnotify/fsnotify"

	"github.com/gfx-labs/towa/src/config"
)

// reparser resolves the configuration again from the command line, the
// environment and the files as they are now.
type reparser func() (*config.Loaded, error)

// fixedSections are the config sections what serve opens at the start
// reads, so a change to them takes a restart.
var fixedSections = []string{"listen.", "store.", "telemetry.", "mcp."}

// configWatch reloads the configuration when one of its files changes or
// the process gets SIGHUP.
type configWatch struct {
	watcher *fsnotify.Watcher
	files   map[string]bool
	hangup  chan os.Signal
}

// watchConfig starts watching the files loaded names. It watches their
// directories, since an editor that saves by renaming replaces the file.
// A directory that is not there is not watched.
func watchConfig(loaded *config.Loaded) (*configWatch, error) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	w := &configWatch{watcher: watcher, files: map[string]bool{}, hangup: make(chan os.Signal, 1)}
	dirs := map[string]bool{}
	for _, path := range loaded.Watched {
		path, err := filepath.Abs(path)
		if err != nil {
			return nil, errors.Join(err, watcher.Close())
		}
		w.files[path] = true
		dirs[filepath.Dir(path)] = true
	}
	for dir := range dirs {
		err := watcher.Add(dir)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, errors.Join(err, watcher.Close())
		}
	}
	signal.Notify(w.hangup, syscall.SIGHUP)
	return w, nil
}

// run reloads until ctx ends. A configuration that does not resolve or
// apply is logged and the one in use stays.
func (w *configWatch) run(ctx context.Context, current *config.Loaded, reparse reparser, logger *slog.Logger, apply func(context.Context, *config.Loaded) error) error {
	defer signal.Stop(w.hangup)
	for {
		select {
		case <-ctx.Done():
			return w.watcher.Close()
		case err := <-w.watcher.Errors:
			logger.Error("config watch", "err", err)
			continue
		case event := <-w.watcher.Events:
			if !w.files[event.Name] || event.Op == fsnotify.Chmod {
				continue
			}
		case <-w.hangup:
		}
		next, err := reparse()
		if err != nil {
			logger.Error("config not reloaded", "err", err)
			continue
		}
		var fixed []string
		for key, value := range next.Values {
			if current.Values[key] != value && slices.ContainsFunc(fixedSections, func(prefix string) bool { return strings.HasPrefix(key, prefix) }) {
				fixed = append(fixed, key)
			}
		}
		if err := apply(ctx, next); err != nil {
			logger.Error("config not reloaded", "err", err)
			continue
		}
		current = next
		slices.Sort(fixed)
		if len(fixed) != 0 {
			logger.Warn("config reloaded, these keys take a restart", "keys", fixed)
		} else {
			logger.Info("config reloaded")
		}
	}
}
