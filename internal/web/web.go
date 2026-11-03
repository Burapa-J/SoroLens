// Package web serves SoroLens's server-rendered explorer.
//
// Pages are plain html/template output with htmx used for the parts that
// benefit from it — filtering and paging an event list without reloading the
// page. There is no build step and no frontend framework: a contributor who
// knows HTML can change any of this.
//
// Like the API, this package depends only on source.EventSource, so it renders
// identically in standalone and upstream mode.
package web

import (
	"embed"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/sorotrail/sorolens/internal/api"
	"github.com/sorotrail/sorolens/internal/config"
	"github.com/sorotrail/sorolens/internal/decode"
	"github.com/sorotrail/sorolens/internal/source"
)

//go:embed templates/*.html
var templatesFS embed.FS

//go:embed favicon.svg
var faviconSVG []byte

// pageNames are the full pages; each is parsed together with the layout and
// the shared partials.
var pageNames = []string{"index", "contracts", "contract", "event", "notfound"}

// Server renders the explorer.
type Server struct {
	src   source.EventSource
	log   *slog.Logger
	pages map[string]*template.Template
	// partials are fragments rendered on their own for htmx swaps.
	partials *template.Template
}

// New parses the templates and returns a ready server.
func New(src source.EventSource, log *slog.Logger) (*Server, error) {
	s := &Server{src: src, log: log, pages: make(map[string]*template.Template, len(pageNames))}

	for _, name := range pageNames {
		t, err := template.New("").Funcs(templateFuncs()).ParseFS(templatesFS,
			"templates/layout.html", "templates/partials.html", "templates/"+name+".html")
		if err != nil {
			return nil, fmt.Errorf("parsing template %s: %w", name, err)
		}
		s.pages[name] = t
	}

	partials, err := template.New("").Funcs(templateFuncs()).ParseFS(templatesFS, "templates/partials.html")
	if err != nil {
		return nil, fmt.Errorf("parsing partials: %w", err)
	}
	s.partials = partials

	return s, nil
}

// Routes returns the UI router, mounted at / by cmd/sorolens.
//
// contributors: new pages go here. Keep them server-rendered; the moment a
// page needs a build step it stops being editable by everyone.
func (s *Server) Routes() chi.Router {
	r := chi.NewRouter()

	r.Get("/", s.index)
	r.Get("/favicon.ico", s.favicon)
	r.Get("/contracts", s.contracts)
	r.Get("/contracts/{id}", s.contract)
	r.Get("/events/{id}", s.event)
	r.Get("/search", s.search)

	// htmx fragment endpoints: these return just a table fragment, so filtering
	// and paging can swap in place when driven via htmx while full pages stay
	// linkable via query-string URLs.
	r.Get("/partials/events", s.partialEvents)
	r.Get("/partials/contracts", s.partialContracts)

	r.NotFound(s.notFound)
	return r
}

// pageData is the common shape every template receives.
type pageData struct {
	Title  string
	Status source.Status
	// Active is the header nav entry this page highlights, set from page
	// via navSection. Empty means no entry is highlighted.
	Active string
	// Data carries the page-specific payload.
	Data any
}

// navSection maps a page name to the header nav entry it highlights.
// "contract" (a contract's detail page) highlights the same entry as
// "contracts" — they're the same section as far as navigation is
// concerned. "event" and "notfound" are deliberately absent (map lookup
// zero-values to ""): neither is reached from a nav link, so nothing
// should highlight for them, and "" must therefore not collide with
// "index"'s own key below.
var navSection = map[string]string{
	"index":     "overview",
	"contracts": "contracts",
	"contract":  "contracts",
}

func (s *Server) render(w http.ResponseWriter, r *http.Request, page, title string, data any) {
	tmpl, ok := s.pages[page]
	if !ok {
		s.log.Error("unknown template", "page", page)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	payload := pageData{
		Title:  title,
		Status: s.src.Status(r.Context()),
		Active: navSection[page],
		Data:   data,
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tmpl.ExecuteTemplate(w, "layout", payload); err != nil {
		// The response is already partly written by this point, so all that is
		// left is to record it.
		s.log.Error("rendering page", "page", page, "error", err)
	}
}

// favicon serves the embedded icon so browsers requesting /favicon.ico on
// every page load stop logging 404s. Served with a long cache lifetime:
// the icon is baked into the binary, so a new version always ships with a
// new deploy anyway.
func (s *Server) favicon(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "image/svg+xml")
	w.Header().Set("Cache-Control", "public, max-age=604800")
	if _, err := w.Write(faviconSVG); err != nil {
		s.log.Warn("writing favicon response", "error", err)
	}
}

func (s *Server) renderPartial(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.partials.ExecuteTemplate(w, name, data); err != nil {
		s.log.Error("rendering partial", "partial", name, "error", err)
	}
}

// fail renders an error page rather than a bare status code, so a failing
// backend still shows the navigation and the status banner.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, action string, err error) {
	s.log.Error(action+" failed", "error", err)
	w.WriteHeader(http.StatusInternalServerError)
	s.render(w, r, "notfound", "Error", map[string]string{
		"Heading": "Something went wrong",
		"Message": action + " failed. Check the service logs for detail.",
	})
}

func (s *Server) notFound(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusNotFound)
	s.render(w, r, "notfound", "Not found", map[string]string{
		"Heading": "Not found",
		"Message": "No page matches " + r.URL.Path + ".",
	})
}

// templateFuncs exposes the rendering helpers to templates.
//
// contributors: this is where a new display helper goes. Anything that decides
// how a decoded value should look belongs in internal/decode instead, so the
// API and the UI agree.
func templateFuncs() template.FuncMap {
	return template.FuncMap{
		// render turns one decoded ScVal into a readable string.
		"render": decode.Render,
		// renderTopics turns a topics array into one string per topic.
		"renderTopics": decode.RenderTopics,
		// eventName is the conventional first-topic symbol, or "" if absent.
		"eventName": decode.EventName,
		// shorten abbreviates long identifiers for table cells.
		"shorten": decode.Shorten,
		// summary renders a value, truncated for a single table cell.
		"summary": func(raw any, limit int) string {
			var text string
			switch v := raw.(type) {
			case string:
				text = v
			default:
				text = fmt.Sprint(v)
			}
			if len(text) <= limit {
				return text
			}
			return text[:limit] + "…"
		},
		// join concatenates strings with a separator.
		"join": strings.Join,
		// add supports simple arithmetic in templates.
		"add": func(a, b int) int { return a + b },
	}
}

// searchKind classifies a search box entry so /search can route it.
type searchKind int

const (
	searchUnknown searchKind = iota
	searchContract
	searchEvent
)

// classifySearch decides whether a query is a contract ID or an event TOID.
// Contract IDs are strkey (C followed by 55 base32 chars); event IDs are
// digit-and-dash TOIDs such as 0001234567890123456-0000000001.
func classifySearch(query string) searchKind {
	query = strings.TrimSpace(query)
	switch {
	case query == "":
		return searchUnknown
	case config.ValidContractID(strings.ToUpper(query)):
		return searchContract
	case looksLikeEventID(query):
		return searchEvent
	default:
		return searchUnknown
	}
}

// looksLikeEventID reports whether s has the shape of an event TOID: digits,
// optionally followed by a dash and more digits.
func looksLikeEventID(s string) bool {
	if s == "" {
		return false
	}
	seenDash := false
	for i, c := range s {
		switch {
		case c >= '0' && c <= '9':
		case c == '-' && i > 0 && !seenDash:
			seenDash = true
		default:
			return false
		}
	}
	return !strings.HasSuffix(s, "-")
}

// errNotFound lets handlers check for a missing record without importing the
// source package's error into every file.
var errNotFound = source.ErrNotFound

func isNotFound(err error) bool { return errors.Is(err, errNotFound) }

func isInvalidCursor(err error) bool {
	return err != nil && (errors.Is(err, source.ErrInvalidCursor) || strings.Contains(err.Error(), "invalid cursor"))
}

// contractQueryFromRequest reuses the API's parser and clamps the page size
// through source.NormalizeLimit so the UI and the API enforce identical bounds.
func contractQueryFromRequest(r *http.Request) (source.ContractQuery, error) {
	q, err := api.ContractQueryFromRequest(r)
	if err != nil {
		return q, err
	}
	q.Limit = source.NormalizeLimit(q.Limit)
	return q, nil
}

// eventQueryFromRequest reuses the API's parser so the UI and the API accept
// exactly the same filters.
func eventQueryFromRequest(r *http.Request) (source.EventQuery, error) {
	return api.EventQueryFromRequest(r)
}
