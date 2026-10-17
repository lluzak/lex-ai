// Command lexai is the whole application in one binary:
//
//	lexai serve                       web + API + background workers + scheduler
//	lexai migrate                     apply database migrations
//	lexai backfill sejmeli [flags]    enqueue a historical import of DU/MP acts
//	lexai fetch DU/2024/1             enqueue one act (handy in development)
//	lexai reparse | rerender          enqueue re-processing of all acts
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"golang.org/x/crypto/acme/autocert"

	"github.com/lluzak/lex-ai/internal/blobstore"
	"github.com/lluzak/lex-ai/internal/config"
	"github.com/lluzak/lex-ai/internal/connectors/sejmeli"
	"github.com/lluzak/lex-ai/internal/db"
	"github.com/lluzak/lex-ai/internal/jobs"
	"github.com/lluzak/lex-ai/internal/search"
	"github.com/lluzak/lex-ai/internal/store"
	"github.com/lluzak/lex-ai/internal/web"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)

	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load()
	if err != nil {
		fail(err)
	}

	cmd, args := os.Args[1], os.Args[2:]
	switch cmd {
	case "serve":
		err = serve(ctx, cfg, log)
	case "migrate":
		err = migrate(ctx, cfg)
	case "backfill":
		err = backfill(ctx, cfg, args)
	case "fetch":
		err = fetch(ctx, cfg, args)
	case "reparse":
		err = enqueueAll(ctx, cfg, func(id int64) river.JobArgs { return jobs.ParseActArgs{DocumentID: id} })
	case "rerender":
		err = enqueueAll(ctx, cfg, func(id int64) river.JobArgs { return jobs.RenderArgs{DocumentID: id} })
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fail(err)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage: lexai <command>

commands:
  serve                                   run web server, workers and scheduler
  migrate                                 apply database migrations
  backfill sejmeli [-pub DU,MP] [-from 1918] [-to <this year>] [-force]
  fetch DU/2024/1 [MP/2023/5 ...]         enqueue single acts
  reparse                                 re-parse all acts (after a parser change)
  rerender                                re-render all act pages`)
}

func fail(err error) {
	slog.Error("fatal", "err", err)
	os.Exit(1)
}

func migrate(ctx context.Context, cfg config.Config) error {
	pool, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := db.Migrate(ctx, pool); err != nil {
		return err
	}
	slog.Info("migrations applied")
	return nil
}

func openBlobs(cfg config.Config) (blobstore.Store, error) {
	if cfg.BlobDriver == "s3" {
		return blobstore.NewS3(blobstore.S3Options{
			Endpoint: cfg.S3Endpoint, Region: cfg.S3Region, Bucket: cfg.S3Bucket,
			AccessKey: cfg.S3AccessKey, SecretKey: cfg.S3SecretKey,
			UseSSL: cfg.S3UseSSL, PathStyle: cfg.S3UsePathStyle,
		})
	}
	return blobstore.NewLocalFS(cfg.BlobLocalDir)
}

func serve(ctx context.Context, cfg config.Config, log *slog.Logger) error {
	pool, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	blobs, err := openBlobs(cfg)
	if err != nil {
		return err
	}
	srv, err := web.New(ctx, pool, &search.Postgres{Pool: pool}, cfg.BaseURL, log)
	if err != nil {
		return err
	}

	pipeline := &jobs.Pipeline{
		Pool:         pool,
		Blobs:        blobs,
		ELI:          sejmeli.NewClient(cfg.SejmELIBaseURL, cfg.UserAgent, cfg.SourceRateLimit),
		FilesBaseURL: cfg.FilesBaseURL,
		OnRendered:   srv.Invalidate,
		Log:          log,
	}
	riverCfg := &river.Config{Workers: jobs.Workers(pipeline), Logger: log}
	if cfg.RunWorkers {
		riverCfg.Queues = jobs.Queues(cfg.Workers)
		riverCfg.PeriodicJobs = jobs.PeriodicJobs(cfg.DeltaInterval)
	}
	rc, err := river.NewClient(riverpgxv5.New(pool), riverCfg)
	if err != nil {
		return err
	}
	pipeline.Enqueue = rc
	if cfg.RunWorkers {
		if err := rc.Start(ctx); err != nil {
			return err
		}
	}

	errc := make(chan error, 2)
	servers := listen(cfg, srv.Handler(), errc)
	log.Info("lexai started", "addr", cfg.Addr, "workers", cfg.RunWorkers, "blobs", cfg.BlobDriver)

	select {
	case <-ctx.Done():
	case err = <-errc:
	}
	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, s := range servers {
		s.Shutdown(shutdownCtx)
	}
	if cfg.RunWorkers {
		if stopErr := rc.Stop(shutdownCtx); stopErr != nil {
			log.Error("stop workers", "err", stopErr)
		}
	}
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// listen starts HTTP(S). Production runs behind Cloudflare with an Origin CA
// certificate; autocert is the fallback for running without Cloudflare.
func listen(cfg config.Config, h http.Handler, errc chan<- error) []*http.Server {
	newServer := func(addr string, h http.Handler) *http.Server {
		return &http.Server{
			Addr: addr, Handler: h,
			ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second,
			WriteTimeout: 60 * time.Second, IdleTimeout: 120 * time.Second,
		}
	}
	switch {
	case cfg.TLSCertFile != "":
		s := newServer(cfg.Addr, h)
		go func() { errc <- s.ListenAndServeTLS(cfg.TLSCertFile, cfg.TLSKeyFile) }()
		return []*http.Server{s}
	case len(cfg.AutocertDomains) > 0:
		m := &autocert.Manager{
			Prompt:     autocert.AcceptTOS,
			HostPolicy: autocert.HostWhitelist(cfg.AutocertDomains...),
			Cache:      autocert.DirCache(cfg.AutocertDir),
		}
		https := newServer(":443", h)
		https.TLSConfig = &tls.Config{GetCertificate: m.GetCertificate, MinVersion: tls.VersionTLS12,
			NextProtos: []string{"h2", "http/1.1", "acme-tls/1"}}
		http80 := newServer(":80", m.HTTPHandler(nil))
		go func() { errc <- https.ListenAndServeTLS("", "") }()
		go func() { errc <- http80.ListenAndServe() }()
		return []*http.Server{https, http80}
	default:
		s := newServer(cfg.Addr, h)
		go func() { errc <- s.ListenAndServe() }()
		return []*http.Server{s}
	}
}

// insertClient is a River client that only inserts jobs (no workers); the
// running `lexai serve` picks them up.
func insertClient(ctx context.Context, cfg config.Config) (*pgxpool.Pool, *river.Client[pgx.Tx], error) {
	pool, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return nil, nil, err
	}
	rc, err := river.NewClient(riverpgxv5.New(pool), &river.Config{})
	if err != nil {
		pool.Close()
		return nil, nil, err
	}
	return pool, rc, nil
}

func backfill(ctx context.Context, cfg config.Config, args []string) error {
	if len(args) == 0 || args[0] != "sejmeli" {
		return errors.New("usage: lexai backfill sejmeli [-pub DU,MP] [-from 1918] [-to YEAR] [-force]")
	}
	fs := flag.NewFlagSet("backfill", flag.ContinueOnError)
	pubs := fs.String("pub", "DU,MP", "publishers")
	from := fs.Int("from", 1918, "first year")
	to := fs.Int("to", time.Now().Year(), "last year")
	force := fs.Bool("force", false, "re-fetch acts even if unchanged")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	pool, rc, err := insertClient(ctx, cfg)
	if err != nil {
		return err
	}
	defer pool.Close()

	var params []river.InsertManyParams
	for _, pub := range strings.Split(*pubs, ",") {
		pub = strings.ToUpper(strings.TrimSpace(pub))
		for y := *to; y >= *from; y-- { // newest first: most useful data arrives first
			params = append(params, river.InsertManyParams{Args: jobs.ListYearArgs{Publisher: pub, Year: y, Force: *force}})
		}
	}
	if _, err := rc.InsertMany(ctx, params); err != nil {
		return err
	}
	slog.Info("backfill enqueued", "years", len(params))
	return nil
}

func fetch(ctx context.Context, cfg config.Config, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: lexai fetch DU/2024/1 [...]")
	}
	pool, rc, err := insertClient(ctx, cfg)
	if err != nil {
		return err
	}
	defer pool.Close()
	var params []river.InsertManyParams
	for _, a := range args {
		parts := strings.Split(a, "/")
		if len(parts) != 3 {
			return fmt.Errorf("invalid act %q, want PUB/YEAR/POS", a)
		}
		year, err1 := strconv.Atoi(parts[1])
		pos, err2 := strconv.Atoi(parts[2])
		if err1 != nil || err2 != nil {
			return fmt.Errorf("invalid act %q", a)
		}
		params = append(params, river.InsertManyParams{Args: jobs.FetchActArgs{
			Publisher: strings.ToUpper(parts[0]), Year: year, Pos: pos, Force: true,
		}})
	}
	_, err = rc.InsertMany(ctx, params)
	return err
}

func enqueueAll(ctx context.Context, cfg config.Config, mk func(int64) river.JobArgs) error {
	pool, rc, err := insertClient(ctx, cfg)
	if err != nil {
		return err
	}
	defer pool.Close()
	ids, err := store.DocumentIDs(ctx, pool, "sejm_eli")
	if err != nil {
		return err
	}
	for start := 0; start < len(ids); start += 1000 {
		end := min(start+1000, len(ids))
		params := make([]river.InsertManyParams, 0, end-start)
		for _, id := range ids[start:end] {
			params = append(params, river.InsertManyParams{Args: mk(id)})
		}
		if _, err := rc.InsertMany(ctx, params); err != nil {
			return err
		}
	}
	slog.Info("enqueued", "documents", len(ids))
	return nil
}
