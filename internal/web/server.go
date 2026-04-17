// Package web serves kennyscope's HTML views over HTTP.
// Server-rendered, no JavaScript, refresh-with-F5.
package web

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/vmorsell/kennyscope/internal/store"
)

//go:embed templates/*.html
var templatesFS embed.FS

type Server struct {
	srv   *http.Server
	store *store.Store
	tmpls *template.Template

	basicAuthUser string
	basicAuthPass string
}

type Config struct {
	Addr          string
	Store         *store.Store
	BasicAuthUser string
	BasicAuthPass string
}

func New(cfg Config) (*Server, error) {
	funcs := template.FuncMap{
		"formatTime": func(t time.Time) string {
			if t.IsZero() {
				return "—"
			}
			return t.UTC().Format("2006-01-02 15:04:05")
		},
		"duration": func(start time.Time, end any) string {
			var endT time.Time
			switch v := end.(type) {
			case time.Time:
				endT = v
			case sql.NullTime:
				if v.Valid {
					endT = v.Time
				}
			}
			if endT.IsZero() {
				endT = time.Now().UTC()
			}
			d := endT.Sub(start).Round(time.Second)
			return d.String()
		},
		"shortID": func(id string) string {
			if len(id) > 12 {
				return id[:12]
			}
			return id
		},
		"rowClass": func(e store.Event) string {
			classes := []string{}
			if e.Stream == "stderr" {
				classes = append(classes, "stderr")
			}
			switch e.Msg {
			case "kenny.boot":
				classes = append(classes, "boot")
			case "kenny.shutdown":
				classes = append(classes, "shutdown")
			}
			return strings.Join(classes, " ")
		},
		"string": func(v any) string { return fmt.Sprint(v) },
		"prettyJSON": func(raw string) template.HTML {
			var obj any
			if err := json.Unmarshal([]byte(raw), &obj); err != nil {
				return template.HTML(template.HTMLEscapeString(raw))
			}
			b, err := json.MarshalIndent(obj, "", "  ")
			if err != nil {
				return template.HTML(template.HTMLEscapeString(raw))
			}
			return template.HTML(template.HTMLEscapeString(string(b)))
		},
	}

	tmpls, err := template.New("").Funcs(funcs).ParseFS(templatesFS, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("parse templates: %w", err)
	}

	s := &Server{
		store:         cfg.Store,
		tmpls:         tmpls,
		basicAuthUser: cfg.BasicAuthUser,
		basicAuthPass: cfg.BasicAuthPass,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.healthz)
	mux.HandleFunc("/", s.authz(s.index))
	mux.HandleFunc("/lives/", s.authz(s.life))

	s.srv = &http.Server{
		Addr:              cfg.Addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	return s, nil
}

func (s *Server) Start() {
	go func() {
		if err := s.srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			_ = err
		}
	}()
}

func (s *Server) Shutdown(ctx context.Context) error {
	return s.srv.Shutdown(ctx)
}

// ------------------- handlers -------------------

func (s *Server) authz(h http.HandlerFunc) http.HandlerFunc {
	if s.basicAuthUser == "" && s.basicAuthPass == "" {
		return h
	}
	return func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok ||
			subtle.ConstantTimeCompare([]byte(user), []byte(s.basicAuthUser)) != 1 ||
			subtle.ConstantTimeCompare([]byte(pass), []byte(s.basicAuthPass)) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="kennyscope"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h(w, r)
	}
}

func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.store.Ping(ctx); err != nil {
		http.Error(w, "store unreachable", http.StatusServiceUnavailable)
		return
	}
	_, _ = w.Write([]byte("ok"))
}

type indexData struct {
	Title       string
	Now         string
	Lives       []store.Life
	CurrentLife *store.Life
}

func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	ctx := r.Context()
	lives, err := s.store.ListLives(ctx, 200)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var current *store.Life
	for i := range lives {
		if !lives[i].EndedAt.Valid {
			current = &lives[i]
			break
		}
	}
	data := indexData{
		Title:       "all lives",
		Now:         time.Now().UTC().Format("2006-01-02 15:04:05 UTC"),
		Lives:       lives,
		CurrentLife: current,
	}
	if err := s.render(w, "index.html", data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

type lifeData struct {
	Title  string
	Now    string
	Life   *store.Life
	Events []store.Event
}

func (s *Server) life(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	idStr := strings.TrimPrefix(r.URL.Path, "/lives/")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	life, err := s.store.GetLife(ctx, id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if life == nil {
		http.NotFound(w, r)
		return
	}
	events, err := s.store.EventsByLife(ctx, id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	data := lifeData{
		Title:  fmt.Sprintf("life #%d", id),
		Now:    time.Now().UTC().Format("2006-01-02 15:04:05 UTC"),
		Life:   life,
		Events: events,
	}
	if err := s.render(w, "life.html", data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (s *Server) render(w http.ResponseWriter, name string, data any) error {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// The page template defines {{block "content" .}} inside layout's "layout"
	// definition. Executing "layout" pulls in the page-specific content block.
	// We associate the page's "content" definition by parsing it last.
	t, err := s.tmpls.Clone()
	if err != nil {
		return err
	}
	t, err = t.ParseFS(templatesFS, "templates/"+name)
	if err != nil {
		return err
	}
	return t.ExecuteTemplate(w, "layout", data)
}
