package jobs_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"github.com/lluzak/lex-ai/internal/blobstore"
	"github.com/lluzak/lex-ai/internal/connectors/sejmeli"
	"github.com/lluzak/lex-ai/internal/db"
	"github.com/lluzak/lex-ai/internal/jobs"
	"github.com/lluzak/lex-ai/internal/search"
	"github.com/lluzak/lex-ai/internal/store"
	"github.com/lluzak/lex-ai/internal/web"
)

// testDB returns a pool on a freshly migrated database, or skips the test
// when LEXAI_TEST_DATABASE_URL isn't set.
func testDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("LEXAI_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LEXAI_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	pool, err := db.Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(ctx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public;
		DROP TEXT SEARCH CONFIGURATION IF EXISTS lexai_pl;`); err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	return pool
}

// fakeELI serves the hand-written fixtures in testdata/sejmeli.
func fakeELI(t *testing.T) (*httptest.Server, *int) {
	t.Helper()
	dir := filepath.Join("..", "..", "testdata", "sejmeli")
	routes := map[string]string{
		"/eli/acts/DU/2024":             "list_DU_2024.json",
		"/eli/acts/DU/2024/1":           "act_DU_2024_1.json",
		"/eli/acts/DU/2024/1/text.html": "text_DU_2024_1.html",
		"/eli/acts/DU/2024/2":           "act_DU_2024_2.json",
	}
	hits := new(int)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*hits++
		if ua := r.Header.Get("User-Agent"); !strings.HasPrefix(ua, "lex-ai-test") {
			t.Errorf("unexpected User-Agent %q", ua)
		}
		name, ok := routes[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		w.Write(data)
	}))
	t.Cleanup(srv.Close)
	return srv, hits
}

func TestPipelineEndToEnd(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	eli, hits := fakeELI(t)
	blobs, err := blobstore.NewLocalFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	srv, err := web.New(ctx, pool, &search.Postgres{Pool: pool}, "http://example.test", log)
	if err != nil {
		t.Fatal(err)
	}
	p := &jobs.Pipeline{
		Pool: pool, Blobs: blobs, Log: log,
		ELI:        sejmeli.NewClient(eli.URL+"/eli", "lex-ai-test", 1000),
		OnRendered: srv.Invalidate,
	}
	rc, err := river.NewClient(riverpgxv5.New(pool), &river.Config{
		Queues:  jobs.Queues(2),
		Workers: jobs.Workers(p),
		Logger:  log,
	})
	if err != nil {
		t.Fatal(err)
	}
	p.Enqueue = rc
	if err := rc.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rc.Stop(context.Background()) })

	// list → 2 fetches → 2 parses → 2 renders = 7 jobs
	completed, cancel := rc.Subscribe(river.EventKindJobCompleted, river.EventKindJobFailed)
	defer cancel()
	if _, err := rc.Insert(ctx, jobs.ListYearArgs{Publisher: "DU", Year: 2024}, nil); err != nil {
		t.Fatal(err)
	}
	waitJobs(t, completed, 7)

	doc, err := store.GetDocumentByELI(ctx, pool, "DU/2024/1")
	if err != nil {
		t.Fatal(err)
	}
	if doc.Title == "" || doc.Status != "obowiązujący" || doc.ParserVersion == 0 || doc.ParseError != "" {
		t.Fatalf("unexpected document %+v", doc)
	}
	units, err := store.ListUnits(ctx, pool, doc.ID)
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, u := range units {
		paths = append(paths, u.Path)
	}
	if got := strings.Join(paths, ","); got != "art.1,art.1/pkt.1,art.1/pkt.2,art.2" {
		t.Fatalf("unit paths = %s", got)
	}
	if !strings.Contains(doc.RenderedHTML, `id="art.1/pkt.1"`) {
		t.Fatalf("rendered html missing unit anchor")
	}

	doc2, err := store.GetDocumentByELI(ctx, pool, "DU/2024/2")
	if err != nil {
		t.Fatal(err)
	}
	if doc2.ParseError == "" {
		t.Errorf("act without text should record a parse note")
	}

	// Relations and keywords from the act metadata.
	rels, err := store.ListRelations(ctx, pool, doc.ID, doc.ExternalID)
	if err != nil || len(rels) != 1 || rels[0].ExternalID != "DU/2004/535" || rels[0].Type != "Akty zmienione" {
		t.Fatalf("relations = %+v, %v", rels, err)
	}

	// ---- web ----
	h := srv.Handler()
	get := func(path string, hdr ...string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		for i := 0; i+1 < len(hdr); i += 2 {
			req.Header.Set(hdr[i], hdr[i+1])
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	page := get("/akt/DU/2024/1")
	if page.Code != 200 || !strings.Contains(page.Body.String(), "Ustawa wchodzi w życie") {
		t.Fatalf("act page: %d %s", page.Code, page.Body.String()[:min(300, page.Body.Len())])
	}
	if etag := page.Header().Get("ETag"); etag == "" || get("/akt/DU/2024/1", "If-None-Match", etag).Code != 304 {
		t.Errorf("expected 304 for matching ETag")
	}
	if code := get("/akt/DU/2024/999").Code; code != 404 {
		t.Errorf("missing act status = %d", code)
	}

	res := get("/search?q=ulga+na+złe+długi", "HX-Request", "true")
	if res.Code != 200 || !strings.Contains(res.Body.String(), "/akt/DU/2024/1#art.1/pkt.1") {
		t.Fatalf("search fragment: %d %s", res.Code, res.Body.String())
	}
	if strings.Contains(res.Body.String(), "<html") {
		t.Errorf("htmx request should get a fragment, not a full page")
	}

	redirect := get("/search?q=Dz.U.+2024+poz.+1")
	if redirect.Code != http.StatusSeeOther || redirect.Header().Get("Location") != "/akt/DU/2024/1" {
		t.Errorf("lookup redirect: %d %s", redirect.Code, redirect.Header().Get("Location"))
	}

	rel := get("/fragments/relations?doc=" + strconv.FormatInt(doc.ID, 10))
	if !strings.Contains(rel.Body.String(), "DU/2004/535") {
		t.Errorf("relations fragment: %s", rel.Body.String())
	}

	api := get("/api/acts/DU/2024/1")
	if api.Code != 200 || !strings.Contains(api.Body.String(), `"path":"art.2"`) {
		t.Errorf("api: %d %s", api.Code, api.Body.String())
	}

	// ---- delta: nothing changed → no new fetches ----
	before := *hits
	n, err := p.ListYear(ctx, "DU", 2024, false)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 || *hits != before+1 {
		t.Errorf("unchanged year enqueued %d fetches (%d requests)", n, *hits-before)
	}
}

func waitJobs(t *testing.T, events <-chan *river.Event, n int) {
	t.Helper()
	timeout := time.After(30 * time.Second)
	for i := 0; i < n; i++ {
		select {
		case ev := <-events:
			if ev.Kind == river.EventKindJobFailed {
				t.Fatalf("job %s failed: %v", ev.Job.Kind, ev.Job.Errors)
			}
		case <-timeout:
			t.Fatalf("timed out after %d/%d jobs", i, n)
		}
	}
}
