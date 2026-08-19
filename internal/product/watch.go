package product

import (
	"context"
	"log/slog"
	"time"

	"github.com/fsnotify/fsnotify"
)

// DefaultDebounce coalesces the burst of events a ConfigMap update produces.
//
// kubelet does not write projected volumes atomically as one event: it stages
// a new directory and swings a symlink, generating several events in quick
// succession. Reloading on each one would parse a half-written directory.
const DefaultDebounce = 2 * time.Second

// Watcher reloads product configuration when the directory changes.
type Watcher struct {
	dir      string
	loader   *Loader
	registry *Registry
	debounce time.Duration
	log      *slog.Logger
	onReload func(LoadResult)
}

// WatchOptions configure a Watcher.
type WatchOptions struct {
	Debounce time.Duration
	Logger   *slog.Logger
	// OnReload fires after every swap, including the initial load. Used to
	// update the config_products_loaded and config_load_errors gauges.
	OnReload func(LoadResult)
}

// NewWatcher builds a watcher over the loader's directory.
func NewWatcher(dir string, loader *Loader, reg *Registry, opts WatchOptions) *Watcher {
	if opts.Debounce <= 0 {
		opts.Debounce = DefaultDebounce
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.OnReload == nil {
		opts.OnReload = func(LoadResult) {}
	}
	return &Watcher{
		dir:      dir,
		loader:   loader,
		registry: reg,
		debounce: opts.Debounce,
		log:      opts.Logger,
		onReload: opts.OnReload,
	}
}

// LoadOnce performs the initial load and swap.
//
// Returns an error only if the directory itself could not be read. Individual
// invalid products are reported through the registry and the OnReload hook,
// not as an error - the Coordinator must stay up and serve the API even when
// every product is invalid, because a crash-looping process cannot tell anyone
// why it is unhappy.
func (w *Watcher) LoadOnce() error {
	res, err := w.loader.Load()
	if err != nil {
		return err
	}
	w.registry.Swap(res)
	w.onReload(res)
	w.logResult(res)
	return nil
}

// Run watches until the context is cancelled.
//
// A watch failure is not fatal: the already-loaded configuration keeps working
// and only hot reload is lost, which is strictly better than exiting.
func (w *Watcher) Run(ctx context.Context) error {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer func() { _ = watcher.Close() }()

	if err := watcher.Add(w.dir); err != nil {
		w.log.Warn("config: cannot watch products directory; hot reload disabled",
			"dir", w.dir, "error", err)
		<-ctx.Done()
		return nil
	}

	var timer *time.Timer
	var timerC <-chan time.Time

	for {
		select {
		case <-ctx.Done():
			if timer != nil {
				timer.Stop()
			}
			return nil

		case event, ok := <-watcher.Events:
			if !ok {
				return nil
			}
			// Ignore pure Chmod: kubelet touches permissions without changing
			// content, and reloading on that is wasted work.
			if event.Op == fsnotify.Chmod {
				continue
			}
			w.log.Debug("config: change detected", "path", event.Name, "op", event.Op.String())
			if timer != nil {
				timer.Stop()
			}
			timer = time.NewTimer(w.debounce)
			timerC = timer.C

		case err, ok := <-watcher.Errors:
			if !ok {
				return nil
			}
			w.log.Warn("config: watch error", "error", err)

		case <-timerC:
			timerC = nil
			res, err := w.loader.Load()
			if err != nil {
				w.log.Error("config: reload failed; keeping previous configuration", "error", err)
				continue
			}
			w.registry.Swap(res)
			w.onReload(res)
			w.logResult(res)
		}
	}
}

func (w *Watcher) logResult(res LoadResult) {
	w.log.Info("config: products loaded",
		"valid", len(res.Valid),
		"invalid", len(res.Invalid),
		"dir", w.dir,
	)
	for _, bad := range res.Invalid {
		// Logged at error because it needs a human, but it does not stop the
		// process or the other products.
		w.log.Error("config: product is invalid; previous version retained if any",
			"file", bad.File, "product", bad.Name, "error", bad.Err.Error())
	}
}
