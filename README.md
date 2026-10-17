# lex-ai

All Polish law in one place: legal acts (Dziennik Ustaw, Monitor Polski), and later tax interpretations from EUREKA and court rulings. It has fast search and is built to be used by AI later. See [PLAN.md](PLAN.md) for the full plan.

The whole system is **one Go binary + PostgreSQL**. Files go to Cloudflare R2 (S3 API).

## Status

| Area | State |
|------|-------|
| Sejm ELI connector (DU, MP): list, act metadata, HTML text, PDF | ✅ implemented. Field names need confirming against the live API, see [docs/sources/sejm-eli.md](docs/sources/sejm-eli.md) |
| Pipeline (River jobs): delta → list year → fetch → parse → render | ✅ |
| Act structure parser (część … tiret, codes with §, regulations, quoted amendments) | ✅ |
| Citation parser (`art. 15 ust. 1 pkt 2 ustawy o CIT`) + alias table | ✅ parser; not yet linked into documents |
| Search: Postgres FTS, exact lookups (`Dz.U. 2024 poz. 1`, `art. 415 k.c.`) | ✅. Polish stemming needs the dictionary files, see [docs/search.md](docs/search.md) |
| Web: pre-rendered act pages, htmx live search, relations fragment, JSON API | ✅ |
| Blob storage: R2/S3 + local disk | ✅ |
| OCR of scanned PDFs, EUREKA, court rulings, AI | ⏳ next phases |

## Development

Requirements: Go 1.26+, PostgreSQL 16 (or Docker), `pdftotext` (poppler-utils) for PDF-only acts.

```sh
docker compose up -d postgres                 # or a local Postgres with user/db "lexai"
go run ./cmd/lexai migrate
go run ./cmd/lexai serve                      # http://localhost:8080, workers included

# in another terminal: import something
go run ./cmd/lexai fetch DU/2024/1            # one act
go run ./cmd/lexai backfill sejmeli -pub DU -from 2024 -to 2024   # a whole year
```

In development, files are stored under `./var/files` (`LEXAI_BLOB_DRIVER=localfs`). All settings are environment variables; see `internal/config/config.go` and `.env.example`.

### Tests

```sh
go test ./...                                                     # unit tests
LEXAI_TEST_DATABASE_URL=postgres://lexai:lexai@localhost:5432/lexai_test?sslmode=disable \
  go test ./...                                                   # + end-to-end pipeline test
```

The end-to-end test (`internal/jobs/pipeline_test.go`) runs a fake ELI server and the real River workers, then checks Postgres and the HTTP handlers. **It drops and recreates the `public` schema of the test database.**

## Commands

```
lexai serve                         web + API + workers + nightly delta
lexai migrate                       apply migrations (River + application)
lexai backfill sejmeli [-pub DU,MP] [-from 1918] [-to YEAR] [-force]
lexai fetch DU/2024/1 [...]         enqueue single acts
lexai reparse                       re-parse all acts after a parser change
lexai rerender                      re-render all act pages after a template change
```

## Layout

```
cmd/lexai/                 entry point and subcommands
internal/config/           environment configuration
internal/db/               pool, migrations (goose, embedded) + River migrations
internal/connectors/       one package per source (sejmeli so far)
internal/jobs/             River job definitions and the ingestion pipeline
internal/parse/act/        act structure parser
internal/parse/htmltext/   HTML → text lines
internal/pdftext/          PDF → text (pdftotext)
internal/citation/         provision references and act aliases
internal/lookup/           exact lookups for the search box
internal/store/            Postgres queries
internal/search/           search.Engine + Postgres FTS
internal/render/           pre-rendered act page bodies
internal/web/              HTTP handlers, templates, embedded assets (htmx 2.0.4)
deploy/                    SQL for the Polish dictionary
docs/                      deployment, search and source notes
testdata/                  source fixtures
```

Deployment on one VPS: [docs/deploy.md](docs/deploy.md).
