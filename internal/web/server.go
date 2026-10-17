// Package web serves the site: pre-rendered document pages, search with htmx
// fragments, a small JSON API and the embedded static assets.
package web

import (
	"bytes"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dgraph-io/ristretto/v2"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lluzak/lex-ai/internal/lookup"
	"github.com/lluzak/lex-ai/internal/render"
	"github.com/lluzak/lex-ai/internal/search"
	"github.com/lluzak/lex-ai/internal/store"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed assets/*
var assetFS embed.FS

type Server struct {
	Pool    *pgxpool.Pool
	Search  search.Engine
	BaseURL string
	Log     *slog.Logger

	tmpl        *template.Template
	assetHashes map[string]string
	pages       *ristretto.Cache[string, page]

	aliasMu sync.RWMutex
	aliases map[string]string
}

// page is a cached, fully rendered document page.
type page struct {
	html []byte
	etag string
}

func New(ctx context.Context, pool *pgxpool.Pool, engine search.Engine, baseURL string, log *slog.Logger) (*Server, error) {
	s := &Server{Pool: pool, Search: engine, BaseURL: baseURL, Log: log}
	if err := s.hashAssets(); err != nil {
		return nil, err
	}
	s.tmpl = template.Must(template.New("").Funcs(template.FuncMap{
		"asset":     s.assetURL,
		"actURL":    ActURL,
		"pathLabel": PathLabel,
		"yearOpts":  yearOptions,
	}).ParseFS(templateFS, "templates/*.html"))

	cache, err := ristretto.NewCache(&ristretto.Config[string, page]{
		NumCounters: 100_000,
		MaxCost:     256 << 20, // 256 MiB of rendered pages
		BufferItems: 64,
	})
	if err != nil {
		return nil, err
	}
	s.pages = cache
	if err := s.ReloadAliases(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Server) ReloadAliases(ctx context.Context) error {
	a, err := store.LoadAliases(ctx, s.Pool)
	if err != nil {
		return err
	}
	s.aliasMu.Lock()
	s.aliases = a
	s.aliasMu.Unlock()
	return nil
}

func (s *Server) aliasMap() map[string]string {
	s.aliasMu.RLock()
	defer s.aliasMu.RUnlock()
	return s.aliases
}

// Invalidate drops a cached page; called by the render job in the same process.
func (s *Server) Invalidate(eli string) {
	s.pages.Del(eli)
}

func (s *Server) Handler() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID, middleware.RealIP, middleware.Recoverer)
	r.Use(middleware.Compress(5, "text/html", "text/css", "application/javascript", "application/json"))
	r.Use(s.logRequests)

	r.Get("/", s.home)
	r.Get("/search", s.search)
	r.Get("/akt/{pub}/{year}/{pos}", s.act)
	r.Get("/fragments/relations", s.relations)
	r.Get("/api/acts/{pub}/{year}/{pos}", s.apiAct)
	r.Get("/assets/{name}", s.asset)
	r.Get("/healthz", s.healthz)
	r.Get("/robots.txt", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write([]byte("User-agent: *\nDisallow: /fragments/\nDisallow: /api/\n"))
	})
	r.NotFound(func(w http.ResponseWriter, r *http.Request) { s.notFound(w, r, "Strona nie istnieje.") })
	return r
}

// ---- layout ----

type layoutData struct {
	Title       string
	Description string
	Canonical   string
	Query       string
	Home        bool
	Body        template.HTML
}

func (s *Server) renderPage(w http.ResponseWriter, status int, ld layoutData, name string, data any) {
	var body bytes.Buffer
	if name != "" {
		if err := s.tmpl.ExecuteTemplate(&body, name, data); err != nil {
			s.serverError(w, err)
			return
		}
		ld.Body = template.HTML(body.String())
	}
	var out bytes.Buffer
	if err := s.tmpl.ExecuteTemplate(&out, "layout", ld); err != nil {
		s.serverError(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	w.Write(out.Bytes())
}

func (s *Server) renderFragment(w http.ResponseWriter, name string, data any) {
	var out bytes.Buffer
	if err := s.tmpl.ExecuteTemplate(&out, name, data); err != nil {
		s.serverError(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(out.Bytes())
}

func (s *Server) serverError(w http.ResponseWriter, err error) {
	s.Log.Error("request failed", "err", err)
	http.Error(w, "Wewnętrzny błąd serwera", http.StatusInternalServerError)
}

func (s *Server) notFound(w http.ResponseWriter, _ *http.Request, msg string) {
	s.renderPage(w, http.StatusNotFound, layoutData{Title: "Nie znaleziono"}, "notfound", msg)
}

func isHTMX(r *http.Request) bool { return r.Header.Get("HX-Request") == "true" }

// ---- handlers ----

func (s *Server) home(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "public, max-age=300")
	s.renderPage(w, http.StatusOK, layoutData{Home: true, Canonical: s.BaseURL + "/",
		Description: "Akty prawne z Dziennika Ustaw i Monitora Polskiego w jednym miejscu."}, "home", nil)
}

type lookupHit struct{ URL, Label string }

type searchData struct {
	Query   search.Query
	Hits    []search.Hit
	Lookup  *lookupHit
	NextURL string
}

func (s *Server) search(w http.ResponseWriter, r *http.Request) {
	v := r.URL.Query()
	q := search.Query{
		Text:      strings.TrimSpace(v.Get("q")),
		Publisher: v.Get("pub"),
		YearFrom:  atoi(v.Get("from")),
		YearTo:    atoi(v.Get("to")),
		Limit:     20,
		Offset:    atoi(v.Get("offset")),
	}
	if q.Publisher != "DU" && q.Publisher != "MP" {
		q.Publisher = ""
	}
	data := searchData{Query: q}

	// Exact references jump straight to the document on a normal submit; while
	// typing (htmx) they're offered as the first result instead.
	if t, ok := lookup.Parse(q.Text, s.aliasMap()); ok {
		target := ActURL(t.ELI, t.Path)
		if !isHTMX(r) {
			http.Redirect(w, r, target, http.StatusSeeOther)
			return
		}
		label := t.ELI
		if t.Path != "" {
			label = PathLabel(t.Path) + " — " + t.ELI
		}
		data.Lookup = &lookupHit{URL: target, Label: label}
	}

	if q.Text != "" {
		hits, err := s.Search.Search(r.Context(), q)
		if err != nil {
			s.serverError(w, err)
			return
		}
		data.Hits = hits
		if len(hits) == q.Limit {
			next := cloneValues(v)
			next.Set("offset", strconv.Itoa(q.Offset+q.Limit))
			data.NextURL = "/search?" + next.Encode()
		}
	}

	w.Header().Set("Vary", "HX-Request")
	if isHTMX(r) {
		s.renderFragment(w, "results", data)
		return
	}
	s.renderPage(w, http.StatusOK, layoutData{Title: q.Text, Query: q.Text}, "search", data)
}

func (s *Server) act(w http.ResponseWriter, r *http.Request) {
	eli, ok := eliFromRoute(r)
	if !ok {
		s.notFound(w, r, "Nieprawidłowy adres aktu.")
		return
	}
	pg, found, err := s.actPage(r.Context(), eli)
	if err != nil {
		s.serverError(w, err)
		return
	}
	if !found {
		s.notFound(w, r, "Akt "+eli+" nie jest jeszcze w bazie.")
		return
	}
	w.Header().Set("ETag", pg.etag)
	w.Header().Set("Cache-Control", "public, max-age=3600, stale-while-revalidate=86400")
	if match := r.Header.Get("If-None-Match"); match != "" && match == pg.etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(pg.html)
}

// actPage returns the full page for an act from the in-process cache, or
// builds it from the pre-rendered body stored in Postgres.
func (s *Server) actPage(ctx context.Context, eli string) (page, bool, error) {
	if pg, ok := s.pages.Get(eli); ok {
		return pg, true, nil
	}
	doc, err := store.GetDocumentByELI(ctx, s.Pool, eli)
	if errors.Is(err, store.ErrNotFound) {
		return page{}, false, nil
	}
	if err != nil {
		return page{}, false, err
	}
	body := doc.RenderedHTML
	if body == "" {
		// Ingested but not rendered yet: render on the fly without units.
		body, _, err = render.Act(render.ActPage{Doc: doc, SourceURL: "https://isap.sejm.gov.pl/"})
		if err != nil {
			return page{}, false, err
		}
	}
	var out bytes.Buffer
	err = s.tmpl.ExecuteTemplate(&out, "layout", layoutData{
		Title:       doc.Title,
		Description: doc.DisplayAddress + " — " + doc.Title,
		Canonical:   s.BaseURL + ActURL(eli, ""),
		Body:        template.HTML(body),
	})
	if err != nil {
		return page{}, false, err
	}
	sum := sha256.Sum256(out.Bytes())
	pg := page{html: out.Bytes(), etag: `"` + hex.EncodeToString(sum[:8]) + `"`}
	if doc.RenderedHTML != "" {
		s.pages.Set(eli, pg, int64(len(pg.html)))
	}
	return pg, true, nil
}

type relationGroup struct {
	Type  string
	Items []store.RelatedDocument
}

func (s *Server) relations(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.URL.Query().Get("doc"), 10, 64)
	if err != nil {
		http.Error(w, "bad doc id", http.StatusBadRequest)
		return
	}
	doc, err := store.GetDocument(r.Context(), s.Pool, id)
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	} else if err != nil {
		s.serverError(w, err)
		return
	}
	rels, err := store.ListRelations(r.Context(), s.Pool, id, doc.ExternalID)
	if err != nil {
		s.serverError(w, err)
		return
	}
	var groups []relationGroup
	for _, rel := range rels {
		label := rel.Type
		if rel.Direction == "in" {
			label = "Wskazują na ten akt: " + rel.Type
		}
		if len(groups) == 0 || groups[len(groups)-1].Type != label {
			groups = append(groups, relationGroup{Type: label})
		}
		groups[len(groups)-1].Items = append(groups[len(groups)-1].Items, rel)
	}
	w.Header().Set("Cache-Control", "public, max-age=600")
	s.renderFragment(w, "relations", groups)
}

type apiUnit struct {
	Type    string `json:"type"`
	Number  string `json:"number"`
	Path    string `json:"path"`
	Heading string `json:"heading,omitempty"`
	Text    string `json:"text,omitempty"`
	Depth   int    `json:"depth"`
}

func (s *Server) apiAct(w http.ResponseWriter, r *http.Request) {
	eli, ok := eliFromRoute(r)
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid act address"})
		return
	}
	doc, err := store.GetDocumentByELI(r.Context(), s.Pool, eli)
	if errors.Is(err, store.ErrNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	} else if err != nil {
		s.serverError(w, err)
		return
	}
	units, err := store.ListUnits(r.Context(), s.Pool, doc.ID)
	if err != nil {
		s.serverError(w, err)
		return
	}
	out := make([]apiUnit, len(units))
	for i, u := range units {
		out[i] = apiUnit{Type: u.Type, Number: u.Number, Path: u.Path, Heading: u.Heading, Text: u.Text, Depth: u.Depth}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"eli":             doc.ELI,
		"title":           doc.Title,
		"display_address": doc.DisplayAddress,
		"type":            doc.ActType,
		"status":          doc.Status,
		"published_at":    doc.PublishedAt,
		"in_force_from":   doc.InForceFrom,
		"url":             s.BaseURL + ActURL(doc.ELI, ""),
		"units":           out,
	})
}

func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.Pool.Ping(ctx); err != nil {
		http.Error(w, "database unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Write([]byte("ok"))
}

// ---- assets ----

func (s *Server) hashAssets() error {
	s.assetHashes = map[string]string{}
	return fs.WalkDir(assetFS, "assets", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := assetFS.ReadFile(p)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		s.assetHashes[path.Base(p)] = hex.EncodeToString(sum[:6])
		return nil
	})
}

// assetURL returns a cache-busting URL; the version changes with the content,
// so assets can be cached for a year.
func (s *Server) assetURL(name string) string {
	return "/assets/" + name + "?v=" + s.assetHashes[name]
}

func (s *Server) asset(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	data, err := assetFS.ReadFile("assets/" + path.Base(name))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	switch path.Ext(name) {
	case ".css":
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
	case ".js":
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	}
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.Write(data)
}

// ---- helpers ----

func eliFromRoute(r *http.Request) (string, bool) {
	pub := strings.ToUpper(chi.URLParam(r, "pub"))
	year, err1 := strconv.Atoi(chi.URLParam(r, "year"))
	pos, err2 := strconv.Atoi(chi.URLParam(r, "pos"))
	if (pub != "DU" && pub != "MP") || err1 != nil || err2 != nil || year < 1900 || pos < 1 {
		return "", false
	}
	return pub + "/" + strconv.Itoa(year) + "/" + strconv.Itoa(pos), true
}

// ActURL is the canonical page URL of an act, optionally anchored at a unit.
func ActURL(eli, unitPath string) string {
	u := "/akt/" + eli
	if unitPath != "" {
		u += "#" + unitPath
	}
	return u
}

// PathLabel turns "art.15/ust.1/pkt.2" into "art. 15 ust. 1 pkt 2".
func PathLabel(p string) string {
	var parts []string
	for _, seg := range strings.Split(p, "/") {
		typ, num, _ := strings.Cut(seg, ".")
		num = render.SupNumber(num)
		switch typ {
		case "art":
			parts = append(parts, "art. "+num)
		case "par":
			parts = append(parts, "§ "+num)
		case "ust":
			parts = append(parts, "ust. "+num)
		case "pkt":
			parts = append(parts, "pkt "+num)
		case "lit":
			parts = append(parts, "lit. "+num)
		case "tiret":
			parts = append(parts, "tiret "+num)
		default:
			parts = append(parts, seg)
		}
	}
	return strings.Join(parts, " ")
}

type yearOpt struct {
	Year     int
	Selected bool
}

func yearOptions(selected int) []yearOpt {
	var out []yearOpt
	for y := time.Now().Year(); y >= 1918; y-- {
		out = append(out, yearOpt{Year: y, Selected: y == selected})
	}
	return out
}

func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

func cloneValues(v url.Values) url.Values {
	out := url.Values{}
	for k, vs := range v {
		out[k] = append([]string(nil), vs...)
	}
	return out
}

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		next.ServeHTTP(ww, r)
		if r.URL.Path == "/healthz" || strings.HasPrefix(r.URL.Path, "/assets/") {
			return
		}
		s.Log.Info("http", "method", r.Method, "path", r.URL.Path, "status", ww.Status(),
			"dur_ms", time.Since(start).Milliseconds(), "htmx", isHTMX(r))
	})
}
