# lex-ai — Build Plan

Goal: one place that holds **all Polish legal acts**, **tax interpretations from the EUREKA system** (Ministry of Finance) and **court rulings** (orzecznictwo), with a searchable database and a UI similar to LEX. The data model is built from day one so an **AI layer** (semantic search, Q&A with citations, MCP/API for agents) can be added later without re-ingesting anything.

> Scope note: we clone the *function* of LEX (aggregating public legal sources), not its content. LEX's commentaries, glosses and editorial annotations are copyrighted by Wolters Kluwer. They must never be scraped. The raw sources below are public. Under Art. 4 of the Polish Copyright Act, legal acts, draft acts and official documents are not covered by copyright.

---

## 1. Data sources

| # | Source | What | Access | Phase |
|---|--------|------|--------|-------|
| 1 | **Sejm ELI API** (`api.sejm.gov.pl/eli`) | Dziennik Ustaw (DU) and Monitor Polski (MP): every act since 1918, with metadata, status, HTML/PDF text, references (amends / amended by / consolidated text / implementing acts), keywords | Official public JSON REST API, no key needed | **1** |
| 2 | **ISAP** (`isap.sejm.gov.pl`) | The same corpus as the ELI API, shown as a website. Fallback only, and for checking our data | HTML | 1 (validation only) |
| 3 | **EUREKA** (`eureka.mf.gov.pl`) | Individual and general tax interpretations, binding rate information (WIS), binding excise information (WIA), explanatory notes (objaśnienia podatkowe), selected court rulings | **No official public API found.** The site's frontend loads its data from JSON endpoints, so we must reverse-engineer those (see §3.2). Contact KIS/MF to ask for an export | **1** |
| 4 | Ministerial and agency gazettes (Dzienniki Urzędowe ministrów) | Ministers' orders and similar internal acts | Mixed: HTML, PDF, ELI on some sites | 3 |
| 5 | Voivodeship gazettes (dzienniki urzędowe województw) | Local law: municipal and county resolutions, tax rates | 16 separate portals, mostly PDF | 3 |
| 6a | **CBOSA** (`orzeczenia.nsa.gov.pl`) | NSA and all 16 WSA (administrative courts): includes nearly every court challenge of a tax interpretation, so it pairs directly with EUREKA | HTML search + document pages, no API | **2** (first court source) |
| 6b | **Supreme Court (SN)** (`sn.pl`, orzecznictwo search) | Judgments, decisions and **resolutions (uchwały)**, incl. "zasady prawne" | HTML / PDF | 2 |
| 6c | **Common courts portal** (`orzeczenia.ms.gov.pl`) | Selected judgments of district, regional and appellate courts | HTML; check for an XML/open-data export | 2 |
| 6d | **Constitutional Tribunal (TK)** (`ipo.trybunal.gov.pl`) | Judgments on constitutionality; operative parts are also published in Dz.U. (link to source 1) | HTML / PDF | 2 |
| 6e | **SAOS** (`saos.org.pl`) | Academic aggregator of SN, TK, KIO, NSA and common courts, with a JSON API and bulk dumps | JSON API. Great for a fast backfill, but **check how fresh its data is**; treat it as a bootstrap, not the main source | 2 (bootstrap) |
| 6f | KIO (`orzeczenia.uzp.gov.pl`), CJEU (CURIA / EUR-Lex), ECHR (HUDOC) | Public procurement rulings, EU and human-rights case law | HTML / APIs | 3 |
| 7 | EUR-Lex / Cellar | EU law that applies in Poland | SPARQL plus REST | 3 |
| 8 | Sejm legislative process API (`api.sejm.gov.pl/sejm`), RCL (`legislacja.gov.pl`) | Bills and draft acts ("upcoming changes") | JSON API (Sejm), HTML (RCL) | 3 |

**Why court rulings matter even without binding precedent.** Poland is a civil-law system, so a ruling binds only the case it decides. In practice, though, case law shows how courts read a provision, and some rulings carry real weight:
- **Uchwały of SN or NSA by an enlarged panel** (7 judges, a full chamber or the whole court). An uchwała of the NSA binds every administrative court panel (art. 269 p.p.s.a.). A "zasada prawna" (legal principle) of the SN binds SN panels.
- **TK judgments** are universally binding (art. 190 of the Constitution).
- A **consistent line of case law** (linia orzecznicza) is what practitioners and tax authorities actually rely on.

The model stores this weight explicitly (§4) so that search and AI can rank a 7-judge uchwała above a single WSA judgment.

**The EUREKA tax interpretations are just as important for the MVP as the acts.** They will be the largest document set to ingest: KIS issues about 25k individual interpretations a year.

---

## 2. Architecture: a Go monolith on Postgres, one VPS

### 2.1 Design principles
1. **Two processes on the server, nothing else:** **one Go binary (`lexai`)** and **one PostgreSQL**. There's no reverse proxy, message broker, cache server, search cluster or frontend build. The binary does its own HTTPS, serves the website, runs the background jobs and the scheduler.
2. **Two places for state, each doing one job:**
   - **Postgres** holds all structured data: documents, full-text search, the job queue, pre-rendered pages, and later vectors.
   - An **S3-compatible bucket** holds files: raw downloads, original PDFs, attachments, OCR outputs and backups. It's a managed external service (e.g. Hetzner Object Storage, Backblaze B2, Cloudflare R2, Scaleway, OVH), not another thing we run on the VPS. The code uses the S3 API only, so any provider works and we can switch with a config change.
3. **The workload shape is "big once, then small".** The history backfill is a one-off job measured in days, limited by polite request rates to government sites. After it, the daily delta is a small trickle of documents, and the server is mostly idle and fast for readers.
4. **Precompute, don't compute per request.** Legal texts rarely change after publication. When a document is ingested, its final HTML is rendered once and stored in Postgres. A page view is one primary-key lookup plus an in-memory cache.
5. **Boring, explicit Go.** Standard library first, generated SQL (sqlc), no ORM, no framework. A new developer can read the whole request path in an afternoon.
6. **Idempotency everywhere.** Every write is keyed by `(source, external_id, content_hash)`. Re-runs and retries are always safe.

### 2.2 System overview

```
                     ┌──────────────────────────────── one VPS ────────────────────────────────┐
                     │                                                                          │
 browser ─ HTTPS ──▶ │  lexai  (single Go binary)                                               │
 (HTML + htmx)       │   ├─ HTTPS: autocert (Let's Encrypt), HTTP/2, gzip/brotli                │
                     │   ├─ web:   document pages (pre-rendered HTML from Postgres + RAM cache) │
                     │   │         search page + htmx fragments (templ)                         │
                     │   │         static assets embedded in the binary (go:embed)              │
                     │   ├─ JSON API  ·  /mcp (Phase 4)                                          │
                     │   ├─ workers: River jobs  fetch → extract → parse → link → render → embed │
                     │   ├─ scheduler: River periodic jobs (nightly delta per source)            │
                     │   └─ admin: River UI, ingest status  (behind basic auth)                  │
                     │                 │                                                        │
                     │                 ▼                                                        │
                     │  PostgreSQL 16+                                                          │
                     │   documents · act units · rulings · interpretations · relations          │
                     │   file metadata (key, sha256) · rendered HTML · FTS (Polish) · trigram   │
                     │   job queue (River) · pgvector (Phase 4)                                 │
                     │                                                                          │
                     │  CLI tools inside the image: pdftotext (poppler), tesseract (pol)        │
                     └───────────────┬───────────────────────────────────┬──────────────────────┘
                                     │ S3 API (raw downloads, PDFs,      │ wal-g: WAL archive +
                                     │ attachments, OCR output)          │ nightly base backup
                                     ▼                                   ▼
                     S3-compatible bucket  `lexai-files`       S3-compatible bucket  `lexai-backup`
                     (versioning on)                           (other provider/region if possible)
                                     ▲
 browser ── PDF download ────────────┘  short-lived presigned URL (or proxied through lexai)
```

### 2.3 What does what

| Concern | Choice | Notes |
|--------|--------|-------|
| Language | **Go 1.23+**: one binary | Web, API, workers, scheduler, crawlers, parsers, MCP |
| HTTPS | `golang.org/x/crypto/acme/autocert` | Let's Encrypt certificates obtained and renewed by the binary itself; no Caddy or nginx |
| HTTP | `net/http` + **chi**, gzip/brotli middleware | HTTP/2 built in |
| HTML | **templ** (type-safe Go templates) | Server-rendered; compiled into the binary |
| Interactivity | **htmx** (vendored, pinned version), embedded with `go:embed` | Live search, filters, tabs, "load more". Pages work without JS |
| CSS | One hand-written stylesheet, embedded | No toolchain |
| DB access | **pgx v5** + **sqlc**, migrations with **goose** (embedded, run by `lexai migrate`) | |
| Jobs + scheduling | **River** (`riverqueue/river`) on Postgres | A job is enqueued in the same transaction as the data that triggers it. River UI is mounted as an admin page inside the binary |
| Full-text search | **Postgres FTS** with a **Polish ispell/hunspell dictionary** (e.g. sjp.pl) + **pg_trgm** + `unaccent` | Stored weighted `tsvector` columns with GIN indexes |
| Files | **S3-compatible bucket** through a small `blobstore` interface: S3 implementation (`minio-go` or `aws-sdk-go-v2`, any endpoint) and a local-disk implementation for development and tests | Content-addressed keys `raw/sha256/ab/cd/<hash>.zst`; Postgres stores only key, hash, size and type. Deduplicated automatically |
| What goes in the bucket | Every raw download (JSON/HTML zstd-compressed), **all original PDFs** and annexes, OCR output | We keep our own copy of everything, so we never depend on a source staying online, and we can re-parse at any time |
| PDF downloads for users | Short-lived **presigned URLs** straight from the bucket (or proxied through `lexai` if the bucket must stay private) | Large files never pass through the VPS |
| Rendered pages | `document.rendered_html` (+ `etag`), written by the `render` job | Served with `ETag` / `Cache-Control`; a ristretto LRU in RAM serves hot pages |
| Cache | In-process **ristretto** | Invalidated on render (same process, no network) |
| Crawling | `net/http` + **goquery**; in-process per-host rate limiter (`x/time/rate`) | `chromedp` only if a portal truly needs a browser |
| PDF / OCR | `pdftotext` and `tesseract` CLIs, called from Go | Tools inside the container image, not services |
| Vectors (Phase 4) | **pgvector** (HNSW) in the same Postgres | LLM and embeddings via hosted APIs (Anthropic Go SDK); no GPU |
| Backups | Postgres: **wal-g** (a single Go binary) → backup bucket. Files: **bucket versioning** + replication to a second bucket/provider (`rclone` cron or the provider's replication) | Point-in-time recovery for the database; files are immutable and content-addressed, so they're easy to copy |
| Monitoring | `slog` logs, `/healthz`, `/metrics`, external uptime ping, River UI | Enough for one box |
| Optional, outside the server | **Cloudflare** free plan | Edge cache + DDoS protection with zero ops; the system doesn't depend on it |

### 2.4 Deployment

- **Server:** one VPS, e.g. **8 vCPU / 32 GB RAM / 0.5 TB NVMe** to start. Most of the RAM is Postgres cache. The local disk holds only Postgres; the files (likely low terabytes, mostly PDFs) live in the bucket, where storage is cheap and grows without resizing the server.
- **Layout:** Docker Compose with **two services**, `postgres` and `lexai`. Alternatively: Postgres from distro packages + `lexai` as a systemd unit. Both are fine; Compose is easier to rebuild from scratch.
- **Release:** CI builds the image → pull on the server → `lexai migrate` → restart. A restart takes about 1 second; the brief gap is acceptable at this scale.
- **Backups:** wal-g archives WAL continuously, plus a nightly base backup, to the backup bucket. Files are already off the server, in a versioned bucket replicated to a second location. The VPS itself holds nothing that can't be rebuilt. **Test a restore every month** onto a fresh VPS; the target is "server lost → site back in under an hour".
- **Security:** SSH keys only; firewall allowing only 80/443; Postgres listening on localhost/socket only; admin pages behind auth; unattended security upgrades.

### 2.5 Ingestion: backfill once, then trickle

- **Pipeline as River jobs:** `fetch → extract (PDF/OCR) → parse → link (citations, relations) → render (HTML into Postgres) → index (tsvector columns) → embed (Phase 4)`. Jobs carry only IDs. Each job kind has its own worker limit, so OCR can't starve the website or the other stages.
- **Backfill (one-off):** `lexai backfill <source> [--from --to]`.
  - It's limited by per-host rate limits, not by our server.
  - It's **resumable**: a cursor per source is stored in Postgres.
  - Use **SAOS bulk dumps** and the **ELI API** for history.
  - If OCR of old scans is slow, run the same binary on a bigger temporary machine against the same database for a few days, then switch it off.
- **Nightly delta:** a River periodic job per source fetches changes since its cursor. Unchanged content stops at the hash check, so a normal night takes minutes.
- **Reprocessing:** a parser fix → bump the parser version → `lexai reparse --source=cbosa`. The raw content is in Postgres, so nothing has to be fetched again.
- **Failures:** retries with backoff, then a discarded job visible in River UI, plus a daily summary. A rising parse-error rate for one source means that site changed.

### 2.6 Frontend: server-rendered HTML + htmx

- **Document pages** (`/akt/DU/1964/16`, `/orzeczenie/II-FSK-1234-21`, `/interpretacja/…`): Go returns `rendered_html` from RAM or Postgres, with `ETag` and long cache headers. Historical versions have stable URLs (`/akt/DU/1964/16@2025-01-01`) and are cacheable forever.
- **Dynamic parts as htmx fragments:**
  - `hx-get="/fragments/case-law?unit=…"` for the "Case law" tab;
  - "interpretations citing this article";
  - live search (`hx-trigger="keyup changed delay:250ms"`), filters and "load more".

  Each fragment is a small HTML response.
- **Search:** a normal form; every state has a real URL (`/search?q=…&type=ruling&court=NSA`), so it's shareable and indexable. htmx swaps only the results list.
- **Lookup by citation:** `Dz.U. 2024 poz. 1`, `II FSK 1234/21` or `art. 415 KC` → redirect straight to the document or article.
- **No JS build, no node_modules.** CSS + `htmx.min.js` are embedded in the binary with content-hashed names and cached for a year.

### 2.7 Speed targets (p95, server side)

| Request | Target | How |
|---------|--------|-----|
| Open a document page | **< 10 ms** (RAM hit), **< 25 ms** (Postgres) | Pre-rendered HTML, PK lookup, ristretto cache |
| htmx fragment ("case law", "citing interpretations") | **< 50 ms** | Precomputed reverse-citation lists per article, covering indexes |
| Lookup by citation / signature | **< 30 ms** | Normalized identifier table (B-tree + trigram) + alias map in memory |
| Full-text search with filters | **< 200 ms** | Stored `tsvector` + GIN, filters first, rank only the top candidates, cache for popular queries. **Benchmark in the spikes on the full ruling corpus** |
| AI answer (Phase 4) | first token **< 1.5 s**, streamed | FTS + pgvector retrieval in parallel goroutines, SSE into an htmx fragment |

**Postgres tuning:**
- `shared_buffers` ≈ 25% RAM, `effective_cache_size` ≈ 70%, `work_mem` sized for ranking queries;
- `ltree` for unit paths;
- `VACUUM ANALYZE` after each bulk load.
- During the backfill: bigger `max_wal_size`, then normal settings afterwards.

### 2.8 If one VPS is ever not enough
Steps in order, each independent, none needed on day one:
1. Turn on Cloudflare caching of document pages.
2. Move to a bigger VPS (vertical scaling goes far).
3. Put Postgres on its own server, then add a read replica for search.
4. Only if Postgres FTS proves insufficient in measurements, try ParadeDB `pg_search` (BM25, still inside Postgres) before any external search engine. The `search.Engine` interface keeps this swap local.
5. Put a CDN in front of the files bucket for PDF downloads if traffic grows (Cloudflare R2 has free egress; others can sit behind Cloudflare).

### 2.9 Repository layout

```
cmd/lexai/              main.go: subcommands serve | migrate | backfill <source> | reparse | rerender
internal/
  connectors/           one package per source: sejmeli, eureka, cbosa, sn, tk, saos, ...
    connector.go        interface: ListChanged(ctx, since) / Fetch(ctx, id) / Parse(raw)
  jobs/                 River job kinds: fetch, extract, parse, link, render, index, embed
  parse/                act structure parser, ruling sections, interpretation sections
  citation/             "art. 15 ust. 1 ustawy o CIT" → act_unit resolver (shared by all sources)
  render/               templ components for document pages → rendered_html
  web/                  handlers: pages, search, htmx fragments, redirects, admin
  api/                  JSON API
  store/                sqlc queries + embedded goose migrations
  search/               search.Engine interface + Postgres FTS implementation
  blobstore/            Store interface: Put/Get/Exists/PresignGet; s3 (any endpoint) + localfs (dev/tests)
  ai/                   (Phase 4) chunking, embeddings, retrieval, RAG, MCP tools
  assets/               css, htmx.min.js, icons (go:embed)
deploy/                 docker-compose.yml (postgres + lexai), wal-g config, systemd unit
testdata/               real source snapshots for golden-file parser tests
```

**Local development:** `docker compose up postgres` (the `localfs` blobstore writes to `./var/files`; to test the real S3 path, add a MinIO container in the dev Compose file only, never in production), then `go run ./cmd/lexai serve` and `go run ./cmd/lexai backfill sejmeli --year 2024`. That's the same setup as production.

---

## 3. Ingestion

### 3.1 Sejm ELI (acts)
1. **Backfill:** for each publisher (`DU`, `MP`) and each year (1918 to now), list the acts. Then fetch each act's details: metadata, `references`, `keywords`, text.
2. **Text:** prefer the `text.html` endpoint (structured; available for newer acts). Fall back to `text.pdf` with PDF→text extraction for older acts, and OCR for scans.
3. **Structure parsing:** split each act into a tree of units: *część → dział → rozdział → art. → ust. → pkt → lit. → tiret*. This is the most important parsing step, because citations, deep links and AI chunks all depend on it.
4. **Relations:** store every reference as a typed edge. Types include amends, amended by, repeals, consolidated text of, legal basis, implementing act. This graph produces the "version history" and "related acts" views.
5. **Delta sync:** run daily. Poll the newest positions in the current year and re-fetch acts whose status or references changed. Use the ELI change feed if it is available; verify this against the live API.
6. **Politeness:** rate-limit requests (for example 2–5 req/s), cache responses, and set a User-Agent with a contact address.

### 3.2 EUREKA (tax interpretations)
1. **Discovery spike (do this first, 1–2 days):** use browser devtools on `eureka.mf.gov.pl` to map the JSON endpoints the SPA calls: search, document list, and the document view at `/informacje/podglad/{id}`. Record the pagination, filters (document type, tax, date, signature) and the document payload format.
2. **Legal and ethical check:** read the site terms and `robots.txt`. Interpretations are official documents, so they are not under copyright. Even so, keep traffic low and email KIS/MF to ask for a bulk export or API.
3. **Backfill:** go through document IDs or search pages by type and date range. Store the raw JSON/HTML.
4. **Normalize:** extract the signature (e.g. `0114-KDIP1-1.4012.123.2024.1.AB`), issue date, issuer, document type, tax type, keywords, the cited legal provisions, and the main sections (stan faktyczny / pytanie / stanowisko wnioskodawcy / ocena stanowiska / uzasadnienie).
5. **Link to acts:** parse cited provisions such as "art. 15 ust. 1 ustawy o CIT" and resolve them to `act_unit` rows (§4). These are the cross-links that make the product useful: from an article you can open "interpretations of this article".
6. **Delta:** run daily and fetch documents published since the last run.

### 3.3 Court rulings (orzecznictwo)
1. **Bootstrap from SAOS:** load its bulk dump or API data for history, then switch each court to its own primary portal for fresh data. Keep the SAOS id as an alias so records are not duplicated.
2. **CBOSA first:** it gives the best value for the tax use case. Crawl by date range and court. Each ruling page has metadata (signature, court, date, judges, type, finality, keywords, cited provisions, related rulings) followed by the sentencja and uzasadnienie.
3. **Normalize:**
   - signature (sygnatura, e.g. `II FSK 1234/21`, `III CZP 11/23`), court, division, ruling type (wyrok / postanowienie / uchwała), panel size, judges, decision date, finality (prawomocny);
   - thesis (teza) if published, sentencja, uzasadnienie;
   - **dissenting opinion** (zdanie odrębne), kept separately.
4. **Link everything:**
   - cited provisions → `act_unit` (same citation resolver as EUREKA);
   - **instance chain:** WSA → NSA cassation, district court → appeal → SN cassation;
   - **interpretation ↔ ruling:** WSA judgments name the challenged interpretation by its signature, so match it to the EUREKA document. Then the interpretation page can show "challenged — WSA annulled it, NSA upheld that".
   - TK judgment ↔ the Dz.U. announcement of its operative part, and ↔ the provision it struck down.
5. **Anonymization and GDPR:** courts publish rulings already anonymized. Store them exactly as published, **never try to de-anonymize** or join them with other personal data, and keep a takedown process for removal requests. Judges' names are public and may be shown.
6. **Delta:** daily crawl of new rulings per portal. Also re-check rulings from the last few months, since finality and the uzasadnienie are often added later.

### 3.4 General rules for all sources
- Every source gets its own Go **connector** package (`internal/connectors/<source>`) implementing the same interface: `ListChanged(ctx, since) → ids`, `Fetch(ctx, id) → raw`, `Parse(raw) → normalized`. Adding a new source means adding one package; the River job pipeline (§2.5) is shared.
- Every fetch is stored raw with a content hash, so we only re-parse when the content changed.
- Ingestion is idempotent. Re-running a job never creates duplicate rows.

---

## 4. Data model (core)

```
source            (id, code: 'sejm_eli' | 'eureka' | ..., base_url)
document          (id, source_id, external_id, doc_type, title, signature/eli_id,
                   publisher, year, position, issued_at, published_at,
                   in_force_from, in_force_to, status, issuer, language,
                   raw_sha256 → file, content_hash, rendered_html, etag,
                   created_at, updated_at)
                   doc_type: act | consolidated_text | interpretation_individual |
                             interpretation_general | wis | wia | explanatory_note | ruling ...
ruling            (document_id PK/FK, court_id, division, signature, ruling_type:
                   judgment|decision|resolution, panel: single|3|7|chamber|full,
                   judges[], decided_at, is_final, binding_force: case_only |
                   binding_on_panels | universally_binding, thesis, has_dissent)
court             (id, name, kind: SN|NSA|WSA|TK|SA|SO|SR|KIO|CJEU|ECHR, city, parent_id)
ruling_section    (id, ruling_id, kind: thesis|operative|reasoning|dissent, ordinal, text)
document_version  (id, document_id, valid_from, valid_to, kind: original|consolidated,
                   based_on_document_id)        -- "as of date X" reading
act_unit          (id, document_version_id, parent_id, unit_type: art|ust|pkt|lit|...,
                   number, path: 'art.15/ust.1/pkt.2', ordinal, text, html)
relation          (id, from_document_id, to_document_id, type, from_unit_id?, to_unit_id?,
                   details)                      -- amends, repeals, legal_basis, consolidates,
                                                 -- appeal_of, cassation_of, challenges_interpretation,
                                                 -- strikes_down (TK → act_unit)
citation          (id, from_document_id, from_span, to_unit_id?, raw_text, resolved: bool)
                                                 -- interpretation/ruling → article links
keyword           (id, name)  +  document_keyword (document_id, keyword_id)
attachment        (id, document_id, kind: pdf|html|annex, source_url, file_sha256)
file              (sha256 PK, bucket_key, content_type, size, compression, fetched_at,
                   source_url)                   -- the bytes live in the S3-compatible bucket
source_cursor     (source_id PK, cursor, updated_at)   -- resumable backfill / delta

-- AI-ready (created now, filled in Phase 4)
chunk             (id, document_id, act_unit_id?, ordinal, text, token_count,
                   heading_path, valid_from, valid_to)
chunk_embedding   (chunk_id, model, embedding vector(N))
```

How this design supports the AI layer:
- **Chunks follow legal structure** (an article, or a paragraph if the article is long), not fixed-size windows. Every chunk carries its exact citation path and validity dates. An answer can then say "art. 15 ust. 1 CIT, in force as of 2025-01-01" and link to it.
- `valid_from` / `valid_to` on versions and chunks let us answer "what was the law on date X", which matters for tax questions.
- The citation graph lets retrieval expand from a hit: interpretation → cited article → amending act, or interpretation → the WSA/NSA ruling on it.
- `binding_force` and `panel` on rulings let retrieval rank authority (TK > enlarged-panel uchwała > NSA/SN > lower courts), not just text similarity.

### Versioning: the hard part
Polish law is published as amendments. A consolidated text (*tekst jednolity*) is published only occasionally, and amendments made after it are not applied to it. Plan:
1. **MVP:** show the original text plus the most recent official *tekst jednolity*. Clearly list the amendments published after that consolidated text ("changes not yet in the consolidated text").
2. **Later:** our own consolidation, applying amendment instructions such as "w art. 5 ust. 2 otrzymuje brzmienie…" to the unit tree. This is a research-grade problem. Use rules first, then LLM-assisted with human review. Never present it as official without a disclaimer.

---

## 5. Search and UI

**MVP features**
- Global search across acts and interpretations, with filters: document type, publisher, year, status (in force / repealed), tax type, issuer, date range.
- Look up an act by citation: "Dz.U. 2024 poz. 1", ELI id, or common name ("ustawa o VAT", "KC", "KPA"). Maintain an alias table for common names.
- Act reader: table of contents tree, deep links to articles (`/akt/DU/1964/16#art.415`), status badge, version timeline, related acts, "interpretations citing this article".
- Interpretation reader: sections, cited provisions (linked), similar interpretations, **court outcome** if it was challenged.
- Ruling reader: thesis, operative part, reasoning, dissent, cited provisions (linked), instance chain (earlier and later rulings in the same case), badge for binding force.
- Article page tab **"Case law"**: rulings citing this article, filterable by court and date, with uchwały and TK judgments pinned on top.
- Search by signature (`II FSK 1234/21`) and by court, judge, ruling type.
- PDF download of the original.

**Search engine details**
- Postgres FTS with a Polish dictionary. There's a stored, weighted `tsvector` (title > thesis > body) on `act_unit`, `ruling_section` and interpretation sections, plus a synonym and abbreviation dictionary (CIT = podatek dochodowy od osób prawnych).
- `pg_trgm` indexes on signatures and titles for fuzzy and partial matches (`II FSK 12`).
- Search at the `act_unit` level, grouped by document, so a result jumps straight to the article.
- The UI is server-rendered with htmx (§2.6); results are swapped in without a page reload.

---

## 6. AI layer (Phase 4, prepared from Phase 1)

1. **Embeddings:** the `embed` pipeline stage (§2.5) embeds each `chunk` in batches and stores it in pgvector. Use a multilingual model that handles Polish well, and benchmark candidates on our own query set before choosing.
2. **Hybrid retrieval:** combine Postgres full-text search and pgvector similarity (both in one SQL query) with reciprocal rank fusion, then a reranker. Apply a temporal filter (`valid_on = date`).
3. **RAG Q&A:** the model answers **only from retrieved sources** and gives a mandatory citation for every claim, linked to the article or interpretation. Add a "not legal advice" disclaimer. If no supporting sources are found, it refuses to answer.
4. **Agent access:** run an **MCP server** that exposes these tools: `search_law`, `get_act`, `get_article(act, path, as_of)`, `search_interpretations`, `get_interpretation`, `search_rulings`, `get_ruling`, `find_citing(article)`. Claude and other agents can then use the database directly.
5. **Enrichment jobs** (offline, using an LLM):
   - interpretation summaries and tags;
   - resolving ambiguous citations;
   - a short thesis for rulings that were published without one;
   - **case-law lines (linie orzecznicze):** cluster rulings on the same provision and issue, then summarize the dominant view, minority views and how the view changed over time, with every claim citing specific signatures. Mark which side the binding uchwały take;
   - a "what changed" summary for each amending act.
6. **Evaluation:** a golden set of about 200 real questions with expected source articles. Track retrieval recall@k and the accuracy of citations in answers. Run it on every model or prompt change.

---

## 7. Phased roadmap

| Phase | Duration (1–2 devs) | Deliverables |
|-------|---------------------|--------------|
| **0. Foundations** | 1 week | Go monorepo (§2.9), Postgres with the Polish FTS dictionary, goose migrations, sqlc, River job pipeline skeleton, templ + htmx base layout with embedded assets, autocert HTTPS, two-service Docker Compose, CI (`go vet`, `golangci-lint`, tests), connector interface, VPS provisioning + backups |
| **0.5 Spikes** | 3–5 days | (a) ELI API: confirm endpoints, the change feed, HTML coverage by year. (b) EUREKA: map the internal JSON API, measure volume, check terms. (c) CBOSA page structure, SAOS freshness and dump format. (d) A structure parser prototype on 20 representative acts |
| **1. Acts MVP** | 3–4 weeks | Full DU+MP backfill, structure parsing, relations, daily delta, Postgres FTS, static act pages, search page with htmx, JSON API |
| **1b. EUREKA MVP** | 2–3 weeks (can run in parallel) | EUREKA connector, backfill, section parsing, citation extraction and linking to articles, UI for interpretations |
| **2. Quality** | 2 weeks | Version timeline, the "changes after consolidated text" view, alias and synonym tables, monitoring (ingest lag, failed jobs) |
| **2b. Case law** | 4–6 weeks | SAOS bootstrap; CBOSA connector (tax focus); SN with uchwały; TK; common courts portal; ruling parser (sections, thesis, dissent); linking to articles, instance chains and EUREKA interpretations; ruling reader and the article "Case law" tab |
| **3. Coverage** | ongoing | Ministerial gazettes, voivodeship gazettes (local law), EUR-Lex, CJEU and ECHR case law, KIO, legislative process |
| **4. AI** | 4–6 weeks | Chunking (built in Phase 1), embeddings, hybrid search, RAG with citations, MCP server, evaluation harness |
| **5. Product** | ongoing | Accounts, saved searches, alerts ("notify me when act X changes or new interpretations of art. Y appear"), annotations, own consolidation |

**MVP definition of done:** a user can search "ulga na złe długi". The results include the relevant article of the VAT act (with its current status) and the related EUREKA interpretations. The user can open either one and follow the links between them. The data is at most 24h old.

**Case law done (Phase 2b):** the same VAT article has a "Case law" tab listing the NSA/WSA rulings that cite it, with any NSA uchwała pinned on top. An interpretation that was challenged in court shows the outcome of that challenge and links to the rulings.

---

## 8. Risks and mitigations

| Risk | Mitigation |
|------|------------|
| EUREKA has no official API; endpoints may change or block us | Keep every raw download in the files bucket, low-rate polite crawling, alert on parser failures, ask MF for an official export |
| Older acts are PDF scans only | OCR (Tesseract with Polish models) with a quality flag; give the original PDF priority in the UI |
| Structure parsing edge cases (annexes, tables, formulas, "art. 15a¹") | Grow a test corpus of real acts; fall back to unit-less text |
| Users mistaking our consolidation or AI answers for official text | Show disclaimers, link to the official source, show the origin of every text version |
| Court portals have no APIs; HTML changes break crawlers | One parser per portal with snapshot tests; SAOS as a backup source; alert when parse rates drop |
| Personal data in rulings (GDPR) | Store only what courts published, no de-anonymization or profiling, takedown process, no indexing of party names that slipped through anonymization (detect and mask) |
| Users reading a single ruling as "the law" | Show binding-force badges, and explain in the UI that in Poland a ruling binds only its own case unless marked otherwise |
| Postgres FTS too slow or not good enough on the full ruling corpus | Benchmark it in the spikes. Fall back to ParadeDB `pg_search` (BM25 still inside Postgres) behind the `search.Engine` interface (§2.8) |
| Single VPS = single point of failure | Files are already off the server in a versioned, replicated bucket; Postgres has wal-g point-in-time backups; monthly restore test; rebuild from Compose + backup in under an hour; optional Cloudflare cache keeps pages readable during short outages |
| File storage costs and egress | zstd-compressed raw files, content-addressed dedupe, presigned URLs, a provider with low or zero egress fees (R2, B2 + Cloudflare); measure total size in the spikes |
| Vendor lock-in on storage | S3 API only behind the `blobstore` interface; moving providers is `rclone sync` + a config change |

---

## 9. Immediate next steps
1. Approve the single-VPS Go architecture (§2). Choose the VPS provider and size (see §2.4).
2. Phase 0: scaffold the repo and the two-service Compose file; provision the VPS, the files bucket and the backup bucket, and set up wal-g.
3. Run the two spikes: the ELI API and EUREKA endpoint mapping. Record the findings in `docs/sources/`.
4. Add a third spike: CBOSA page structure and SAOS freshness/dump format. Also benchmark Postgres FTS with the Polish dictionary on a large sample of rulings.
5. Build the ELI connector and backfill one year (e.g. DU 2024) end-to-end before scaling to all years.
