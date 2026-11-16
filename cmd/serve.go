package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"

	"github.com/asciimoth/locks"
)

func (a *app) serve(ctx context.Context) int {
	client, err := locks.OpenWithOptions(locks.ClientOptions{
		Locations: []locks.Location{},
		Prepend:   []locks.Location{{Address: a.options.paths[0]}},
		Local:     locks.LocalOptions{ReadOnly: a.options.readOnly, LockTimeout: a.options.timeout},
	})
	if err != nil {
		return a.fail(err)
	}
	store := client.Chain()[0].Store
	if a.options.appendOnly {
		store = appendStore{store}
	}
	handler, err := locks.NewHandler(store, locks.HandlerOptions{ReadOnly: a.options.readOnly})
	if err != nil {
		return a.fail(err)
	}
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", a.options.listen)
	if err != nil {
		return a.fail(err)
	}
	defer listener.Close()
	if _, err := fmt.Fprintf(a.out, "http://%s\n", listener.Addr()); err != nil {
		return a.fail(err)
	}
	server := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/lock" {
				http.NotFound(w, r)
				return
			}
			requestCtx, cancel := context.WithTimeout(r.Context(), a.options.timeout)
			defer cancel()
			handler.ServeHTTP(w, r.WithContext(requestCtx))
		}),
		ReadHeaderTimeout: a.options.timeout,
		ReadTimeout:       a.options.timeout,
		WriteTimeout:      a.options.timeout,
		IdleTimeout:       a.options.timeout,
		ErrorLog:          log.New(a.errOut, "locks: ", 0),
	}
	done := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		select {
		case <-ctx.Done():
			shutdownCtx, cancel := context.WithTimeout(context.Background(), a.options.timeout)
			defer cancel()
			if err := server.Shutdown(shutdownCtx); err != nil {
				server.Close()
			}
		case <-done:
		}
	}()
	err = server.Serve(listener)
	close(done)
	<-stopped
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return a.fail(err)
	}
	return 0
}

// appendStore uses the local store's atomic create check for every write mode.
type appendStore struct{ locks.Store }

func (s appendStore) Put(ctx context.Context, id locks.ID, value string, _ locks.Mode) (locks.Result, error) {
	result, err := s.Store.Put(ctx, id, value, locks.Create)
	if err != nil {
		return result, err
	}
	if result.Status == locks.Same || result.Status == locks.Conflict {
		return locks.Result{ID: id}, locks.ErrDenied
	}
	return result, nil
}
