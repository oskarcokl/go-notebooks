// Package server provides the web UI and JSON API of a notebook.
package server

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"go-notebooks/internal/manager"
)

//go:embed web
var web embed.FS

// New returns the handler for a notebook. Background work ends with ctx.
func New(ctx context.Context, e *manager.Engine) http.Handler {
	s := &server{e: e, ctx: ctx}
	mux := http.NewServeMux()
	static, _ := fs.Sub(web, "web")
	mux.Handle("GET /", http.FileServerFS(static))
	mux.HandleFunc("GET /api/notebook", s.notebook)
	mux.HandleFunc("GET /api/events", s.events)
	mux.HandleFunc("POST /api/cells", s.insert)
	mux.HandleFunc("GET /api/cells/{id}", s.cell)
	mux.HandleFunc("PATCH /api/cells/{id}", s.update)
	mux.HandleFunc("PUT /api/cells/{id}", s.update)
	mux.HandleFunc("DELETE /api/cells/{id}", s.remove)
	mux.HandleFunc("POST /api/cells/{id}/move", s.move)
	mux.HandleFunc("POST /api/cells/{id}/run", s.run)
	mux.HandleFunc("POST /api/run-all", s.runAll)
	mux.HandleFunc("POST /api/run-stale", s.runStale)
	mux.HandleFunc("GET /api/variables", s.variables)
	mux.HandleFunc("POST /api/build", s.build)
	mux.HandleFunc("POST /api/kernel/restart", s.restart)
	mux.HandleFunc("POST /api/kernel/sync-deps", s.sync)
	return guard(mux)
}

// guard keeps other web sites from reaching the server through the user's
// browser: it runs arbitrary code by design. Requests must address it by a
// loopback name and, unless they only read, come from its own pages.
func guard(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if name, _, err := net.SplitHostPort(host); err == nil {
			host = name
		}
		ip := net.ParseIP(strings.Trim(host, "[]"))
		if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		if o := r.Header.Get("Origin"); o != "" {
			u, err := url.Parse(o)
			if err != nil || u.Host != r.Host {
				http.Error(w, "forbidden origin", http.StatusForbidden)
				return
			}
		}
		if site := r.Header.Get("Sec-Fetch-Site"); r.Method != http.MethodGet && site != "" && site != "same-origin" && site != "none" {
			http.Error(w, "forbidden site", http.StatusForbidden)
			return
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data: https:; frame-ancestors 'none'")
		h.ServeHTTP(w, r)
	})
}

type server struct {
	e   *manager.Engine
	ctx context.Context
}

func reply(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, status int, err error) {
	var nf manager.NotFoundError
	if errors.As(err, &nf) {
		status = http.StatusNotFound
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}

// body decodes an optional JSON body.
func body(w http.ResponseWriter, r *http.Request, v any) bool {
	err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<20)).Decode(v)
	if err != nil && !errors.Is(err, io.EOF) {
		fail(w, http.StatusBadRequest, fmt.Errorf("invalid JSON body: %w", err))
		return false
	}
	return true
}

func (s *server) notebook(w http.ResponseWriter, r *http.Request) { reply(w, s.e.View()) }

func (s *server) cell(w http.ResponseWriter, r *http.Request) {
	v, err := s.e.Cell(r.PathValue("id"))
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	reply(w, v)
}

func (s *server) insert(w http.ResponseWriter, r *http.Request) {
	in := struct{ After, Kind, Title, Source string }{Kind: "code"}
	if !body(w, r, &in) {
		return
	}
	v, err := s.e.Insert(in.After, in.Kind, in.Title, in.Source)
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(v)
}

func (s *server) update(w http.ResponseWriter, r *http.Request) {
	var in struct{ Kind, Title, Source *string }
	if !body(w, r, &in) {
		return
	}
	v, err := s.e.Update(r.PathValue("id"), in.Kind, in.Title, in.Source)
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	reply(w, v)
}

func (s *server) remove(w http.ResponseWriter, r *http.Request) {
	if err := s.e.Delete(r.PathValue("id")); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) move(w http.ResponseWriter, r *http.Request) {
	var in struct{ Delta int }
	if !body(w, r, &in) {
		return
	}
	if err := s.e.Move(r.PathValue("id"), in.Delta); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	reply(w, s.e.View())
}

// Runs use the server's context: closing the page must not cut a cell short.
func (s *server) run(w http.ResponseWriter, r *http.Request) {
	var in struct{ Source *string }
	if !body(w, r, &in) {
		return
	}
	v, err := s.e.RunCell(s.ctx, r.PathValue("id"), in.Source)
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	reply(w, v)
}

func (s *server) runAll(w http.ResponseWriter, r *http.Request) {
	ok := s.e.RunAll(s.ctx)
	reply(w, map[string]any{"ok": ok, "notebook": s.e.View()})
}

func (s *server) runStale(w http.ResponseWriter, r *http.Request) {
	ids, ok := s.e.RunStale(s.ctx)
	reply(w, map[string]any{"ok": ok, "cells": ids})
}

func (s *server) variables(w http.ResponseWriter, r *http.Request) {
	v, t := s.e.Variables()
	reply(w, map[string]any{"variables": v, "types": t})
}

func (s *server) build(w http.ResponseWriter, r *http.Request) {
	var in struct{ Output string }
	if !body(w, r, &in) {
		return
	}
	res, err := s.e.GoBuild(s.ctx, in.Output)
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	reply(w, res)
}

func (s *server) restart(w http.ResponseWriter, r *http.Request) {
	if err := s.e.Restart(s.ctx); err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	reply(w, s.e.View().Kernel)
}

func (s *server) sync(w http.ResponseWriter, r *http.Request) {
	in := struct{ Replay bool }{Replay: true}
	if !body(w, r, &in) {
		return
	}
	rebuilt, err := s.e.SyncDeps(s.ctx, in.Replay)
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	reply(w, map[string]any{"rebuilt": rebuilt, "kernel": s.e.View().Kernel})
}

// events streams changes as server-sent events.
func (s *server) events(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	ch, cancel := s.e.Subscribe()
	defer cancel()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	fmt.Fprint(w, ": connected\n\n")
	fl.Flush()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	beats := 0
	for {
		select {
		case <-r.Context().Done():
			return
		case <-s.ctx.Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			b, _ := json.Marshal(ev)
			fmt.Fprintf(w, "data: %s\n\n", b)
			fl.Flush()
		case <-tick.C:
			s.e.Poll() // notice edits made in other editors
			if beats++; beats%15 == 0 {
				fmt.Fprint(w, ": ping\n\n")
				fl.Flush()
			}
		}
	}
}
