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

## 2. Architecture (Go, one VPS, fast by design)

### 2.1 Design principles
1. **One server, few moving parts.** The whole system is **one Go binary + PostgreSQL + Caddy** on a single VPS. No Kubernetes, message broker, Redis, or separate search cluster. Fewer moving parts means fewer things that break at 3 a.m.
2. **The workload shape is "big once, then small".** The historical backfill is a one-off job measured in days. After that, the daily delta is maybe a few thousand new documents (acts, interpretations, rulings). The design optimizes for this: an efficient, resumable backfill, then a near-idle server that is fast for readers.
3. **Precompute, don't compute per request.** Legal texts rarely change after publication. At ingest time, each act, article, ruling and interpretation is rendered into **static HTML files on disk**. Caddy serves them directly; Go and Postgres aren't involved in a page view at all.
4. **Postgres does everything it can.** It is the source of truth, the **job queue**, the **full-text search engine**, and (Phase 4) the **vector store**. One process to tune, back up and monitor.
5. **Every heavy part sits behind a Go interface** (`search.Engine`, `queue`, `blobstore`). If one day we outgrow the single server, we can swap in OpenSearch, S3 or a second machine without rewriting the application.
6. **Idempotency everywhere.** Every write is keyed by `(source, external_id, content_hash)`. Re-runs and retries are always safe.

### 2.2 System overview

```
                       ┌──────────────────────────────── one VPS ────────────────────────────────┐
                       │                                                                          │
 browser ── HTTPS ───▶ │  Caddy (TLS, gzip/brotli, HTTP/2-3)                                      │
 (htmx)                │    ├─ /akt/… /orzeczenie/… /interpretacja/…  ─▶  static HTML on disk     │
                       │    │                                             (pre-rendered, ~ms)      │
                       │    └─ /search, /fragments/…, /api/…, /mcp  ─▶  lexai (Go binary)          │
                       │                                                 ├─ web: html/template or  │
                       │                                                 │   templ + htmx fragments │
                       │                                                 ├─ JSON API               │
                       │                                                 ├─ workers (River jobs)   │
                       │                                                 │   fetch → extract →     │
                       │                                                 │   parse → link → render │
                       │                                                 ├─ scheduler (daily delta)│
                       │                                                 └─ (Phase 4) AI + MCP     │
                       │                                                        │                  │
                       │                         PostgreSQL 16+ ◀───────────────┘                  │
                       │                         data · FTS (Polish) · job queue · pgvector         │
                       │                                                                          │
                       │  /data/raw    content-addressed raw downloads (zstd-compressed)           │
                       │  /data/site   pre-rendered HTML                                           │
                       └──────────────────────────────┬───────────────────────────────────────────┘
                                                      │ nightly
                                                      ▼
                                    off-site backup (S3-compatible bucket / storage box)
                                    Postgres: pgBackRest or wal-g · files: restic
```

### 2.3 Technology choices

| Concern | Choice | Why |
|--------|--------|-----|
| Language | **Go 1.23+**: one binary for web, API, workers, scheduler, crawlers, parsers and the MCP server | Fast, cheap concurrency, a single static binary to deploy |
| HTTP | `net/http` + **chi** router | Standard library speed, no framework lock-in |
| HTML | **templ** (type-safe Go templates) or `html/template` | Server-rendered HTML, no JS build step |
| Interactivity | **htmx** (plus a few lines of Alpine.js only if really needed), vendored as static files | Search-as-you-type, filters, tabs and "load more" without a SPA. Pages still work without JS |
| CSS | One small hand-written stylesheet (or a classless base like Pico.css) | No CSS toolchain |
| DB access | **pgx v5** + **sqlc**, migrations with **goose** | Type-safe SQL generated at build time, no ORM |
| Job queue + scheduling | **River** (`riverqueue/river`): Go job queue on Postgres, with periodic jobs | No broker to run. A job is inserted in the same transaction as the data that triggers it, so nothing gets lost |
| Full-text search | **Postgres FTS** with a **Polish ispell/hunspell dictionary** (e.g. from sjp.pl) + **pg_trgm** for signatures and fuzzy matching | Handles Polish inflection; GIN indexes; no extra service. Upgrade path below if it isn't enough |
| Cache | In-process **ristretto** LRU for hot search results and fragments | No Redis needed on one box |
| Crawling | `net/http` + **goquery**; **chromedp** only if a portal requires a real browser | Most sources are plain HTML/JSON |
| PDF / OCR | **poppler `pdftotext`** and **Tesseract (`pol`)** installed on the server, called from Go | Go has no mature PDF/OCR libraries |
| Raw storage | Local disk, content-addressed (`/data/raw/sha256/ab/cd/…`), zstd-compressed | Cheap, natural de-duplication, re-parse at any time without refetching |
| Reverse proxy | **Caddy** | Automatic HTTPS, serves static files very fast, a few lines of config |
| Optional | **Cloudflare** (free plan) in front | Free edge cache and DDoS protection for static pages, and nothing to operate |
| AI (Phase 4) | **pgvector** in the same Postgres; LLM and embeddings through hosted APIs (**Anthropic Go SDK**); **MCP Go SDK** | No GPU needed on the VPS |
| Monitoring | `slog` structured logs (journald), `/healthz`, `/metrics` (Prometheus format), an external uptime check, River's job dashboard | Enough for one box; add Grafana later only if wanted |

### 2.4 Deployment

- **Server:** one VPS or a small dedicated server. A reasonable starting point is **8 vCPU, 32 GB RAM, 1–2 TB NVMe**. RAM mostly goes to Postgres cache. Raw PDFs drive the disk size; check that in the spikes.
- **Install:** pick one of these.
  - **Docker Compose** with three services (`postgres`, `lexai`, `caddy`), or
  - **systemd** units, with Postgres and Caddy from distro packages and `lexai` as a single binary.

  Both are fine. Compose is easier to rebuild from scratch.
- **Release:** CI builds the binary or image → `scp`/pull → `lexai migrate` → restart. Deploys take about 1 second; a brief restart is acceptable at this scale. Add zero-downtime deploys later with Caddy health checks if needed.
- **Backups:**
  - Postgres: continuous WAL archiving + nightly base backup to off-site S3-compatible storage (pgBackRest or wal-g).
  - `/data/raw`: nightly `restic`.
  - `/data/site`: not backed up, since it can be regenerated from the database.
  - **Test a restore every month.**
- **Security:** SSH keys only, firewall (only 80/443 open), unattended security upgrades, Postgres listening only on localhost.

### 2.5 Ingestion: backfill once, then trickle

- **One pipeline, as River jobs:** `fetch → extract (PDF/OCR) → parse → link (citations, relations) → render (static HTML) → index (FTS columns) → embed (Phase 4)`. Each job carries only IDs and file paths. Each job type has its own worker count, so the CPU-heavy stages don't starve the rest.
- **Backfill (one-off):**
  - It is limited by **polite request rates to government sites**, not by our server. One VPS is plenty: a few goroutines per source, each source behind its own rate limiter (e.g. 2–5 req/s per host).
  - It is **resumable**: progress is stored per source as a cursor, so a crash or redeploy continues where it stopped.
  - Use **SAOS bulk dumps** and the **ELI API** to make the history fast.
  - If OCR of old scans is slow, run the `extract` stage on a bigger temporary machine (or a laptop) against the same raw files, then copy the results back. That's possible because every stage is idempotent.
- **Daily delta:** a River periodic job per source (e.g. at night) asks each connector for changes since its cursor. Unchanged content is dropped at the hash check, so a normal day's run takes minutes and the server is otherwise idle.
- **Reprocessing:** a parser fix means bumping the parser version, then enqueueing `parse` jobs for the affected raw files. It runs in the background while the site keeps serving.
- **Failures:** retries with backoff. After that a job is marked discarded, and the River UI and a daily summary list the failures. A sudden rise in parse errors for one source means the site changed its layout.

### 2.6 Frontend: static HTML + htmx

- **Document pages are static files.** The `render` stage writes `/data/site/akt/DU/1964/16/index.html`, `/data/site/orzeczenie/II-FSK-1234-21/index.html` and so on. Caddy serves them with long cache headers. Rendering happens only when a document changes.
- **Dynamic parts are small htmx fragments from Go.** A static page includes placeholders that load from Go:
  - `hx-get="/fragments/case-law?unit=…"` for the "Case law" tab;
  - "interpretations citing this article";
  - the search box with live results (`hx-trigger="keyup changed delay:250ms"`);
  - filters;
  - pagination ("load more").

  Each fragment is a small, cacheable HTML response, under 50 ms.
- **Search page:** server-rendered results with filters as normal links/forms. htmx swaps only the results list. Every state has a real URL (`/search?q=…&type=ruling&court=NSA`), so results are shareable and indexable.
- **Lookup by citation:** typing `Dz.U. 2024 poz. 1`, `II FSK 1234/21` or `art. 415 KC` redirects straight to the document or article.
- **No JS build, no node_modules.** Assets are one CSS file, `htmx.min.js` (pinned version, vendored) and a few icons. They're served with content hashes in filenames and cached for a year.
- **SEO and accessibility for free:** real HTML and real links; pages work without JavaScript.

### 2.7 Speed targets (p95, server side, on one VPS)

| Request | Target | How |
|---------|--------|-----|
| Open an act / article / ruling / interpretation | **< 10 ms** | Static file from disk via Caddy (page cache in RAM) |
| htmx fragment ("case law", "citing interpretations") | **< 50 ms** | Precomputed reverse-citation lists per article in Postgres + in-process cache |
| Lookup by citation or signature | **< 30 ms** | Normalized identifier table with B-tree/trigram indexes + alias map in memory |
| Full-text search with filters | **< 200 ms** | Postgres FTS: stored `tsvector` columns with GIN indexes, filters applied first, ranking only on the top candidates, cache for popular queries. **Benchmark this in the spikes on the full ruling corpus** |
| AI answer (Phase 4) | first token **< 1.5 s**, streamed | Retrieval in parallel goroutines, streamed over SSE to an htmx fragment |

**Postgres tuning for this box:**
- `shared_buffers` ≈ 25% RAM, `effective_cache_size` ≈ 70%;
- the big tables (`act_unit`, `ruling_section`, `chunk`) with stored `tsvector` columns and GIN indexes;
- `ltree` for unit paths;
- covering indexes for the hot fragment queries;
- regular `VACUUM ANALYZE` after bulk loads.

### 2.8 When one VPS is no longer enough (upgrade path, not day one)
The order matters, because each step is independent and cheap:
1. Turn on the **Cloudflare cache** for static pages. This removes most of the traffic.
2. Move to a **bigger VPS**. Vertical scaling goes a long way here.
3. Move **Postgres to its own server**, then add a read replica for search queries.
4. If Postgres FTS quality or speed becomes the limit, put **OpenSearch** (Polish Stempel analyzer) or **ParadeDB `pg_search`** (BM25 inside Postgres) behind the `search.Engine` interface.
5. Move raw files to S3-compatible storage behind the `blobstore` interface.

### 2.9 Repository layout (Go monorepo)

```
cmd/lexai/              main: subcommands serve | worker | migrate | backfill <source> | render-all
internal/
  connectors/           one package per source: sejmeli, eureka, cbosa, sn, tk, saos, ...
    connector.go        interface: ListChanged(ctx, since) / Fetch(ctx, id) / Parse(raw)
  jobs/                 River job definitions: fetch, extract, parse, link, render, index, embed
  parse/                act structure parser, ruling sections, interpretation sections
  citation/             "art. 15 ust. 1 ustawy o CIT" → act_unit resolver (shared by all sources)
  render/               static page renderer (writes /data/site)
  web/                  handlers for search, htmx fragments, redirects; templ components
  api/                  JSON API (OpenAPI spec in api/openapi.yaml)
  store/                sqlc queries, migrations
  search/               search.Engine interface + Postgres FTS implementation
  blobstore/            raw file storage (local disk now, S3 later)
  ratelimit/            per-host rate limiters
  ai/                   (Phase 4) chunking, embeddings, retrieval, RAG, MCP tools
web/static/             css, htmx.min.js, icons
deploy/                 docker-compose.yml, Caddyfile, systemd units, backup scripts
testdata/               real source snapshots for golden-file parser tests
```

**Local development:** `docker compose up postgres`, then `go run ./cmd/lexai serve` and `go run ./cmd/lexai backfill sejmeli --year 2024`. Production is the same setup, so there are no surprises.

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
                   raw_object_key, content_hash, created_at, updated_at)
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
attachment        (id, document_id, kind: pdf|html|annex, object_key)

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
| **0. Foundations** | 1 week | Go monorepo (§2.9), Postgres with the Polish FTS dictionary, goose migrations, sqlc, River job pipeline skeleton, templ + htmx base layout, Caddyfile, Docker Compose, CI (`go vet`, `golangci-lint`, tests), connector interface, VPS provisioning + backups |
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
| EUREKA has no official API; endpoints may change or block us | Keep the raw store, low-rate polite crawling, alert on parser failures, ask MF for an official export |
| Older acts are PDF scans only | OCR (Tesseract with Polish models) with a quality flag; give the original PDF priority in the UI |
| Structure parsing edge cases (annexes, tables, formulas, "art. 15a¹") | Grow a test corpus of real acts; fall back to unit-less text |
| Users mistaking our consolidation or AI answers for official text | Show disclaimers, link to the official source, show the origin of every text version |
| Court portals have no APIs; HTML changes break crawlers | One parser per portal with snapshot tests; SAOS as a backup source; alert when parse rates drop |
| Personal data in rulings (GDPR) | Store only what courts published, no de-anonymization or profiling, takedown process, no indexing of party names that slipped through anonymization (detect and mask) |
| Users reading a single ruling as "the law" | Show binding-force badges, and explain in the UI that in Poland a ruling binds only its own case unless marked otherwise |
| Postgres FTS too slow or not good enough on the full ruling corpus | Benchmark it in the spikes. Fall back to ParadeDB `pg_search` or OpenSearch behind the `search.Engine` interface (§2.8) |
| Single VPS = single point of failure | Static pages behind the Cloudflare cache keep reads working through short outages; off-site backups with monthly restore tests; rebuild a server from Compose + backups in under an hour |
| Disk fills up during the backfill (PDFs, OCR) | zstd compression, monitor disk usage, size the disk after the spikes, optional move of raw files to object storage (§2.8) |

---

## 9. Immediate next steps
1. Approve the single-VPS Go architecture (§2). Choose the VPS provider and size (see §2.4).
2. Phase 0: scaffold the repo, Compose file and Caddyfile; provision the VPS with backups.
3. Run the two spikes: the ELI API and EUREKA endpoint mapping. Record the findings in `docs/sources/`.
4. Add a third spike: CBOSA page structure and SAOS freshness/dump format. Also benchmark Postgres FTS with the Polish dictionary on a large sample of rulings.
5. Build the ELI connector and backfill one year (e.g. DU 2024) end-to-end before scaling to all years.
