package internal

import (
	"log/slog"
	"time"

	"github.com/fsnotify/fsnotify"
)

type Watcher struct {
	path     string
	callback func()
	watcher  *fsnotify.Watcher
}

func NewWatcher(path string, callback func()) (*Watcher, error) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	if err := w.Add(path); err != nil {
		w.Close()
		return nil, err
	}
	return &Watcher{path: path, callback: callback, watcher: w}, nil
}

func (w *Watcher) Watch() {
	var lastEvent time.Time

	for {
		select {
		case event, ok := <-w.watcher.Events:
			if !ok {
				return
			}
			if event.Has(fsnotify.Write) || event.Has(fsnotify.Create) {
				now := time.Now()
				if now.Sub(lastEvent) < 500*time.Millisecond {
					continue
				}
				lastEvent = now
				slog.Info("config file changed, reloading", "path", w.path)
				w.callback()
			}
		case err, ok := <-w.watcher.Errors:
			if !ok {
				return
			}
			slog.Error("watcher error", "error", err)
		}
	}
}

func (w *Watcher) Close() error {
	return w.watcher.Close()
}
