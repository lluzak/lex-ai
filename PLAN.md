# lex-ai — Build Plan

Goal: one place that holds **all Polish legal acts** plus **tax interpretations from the EUREKA system** (Ministry of Finance), with a searchable database and a UI similar to LEX. The data model is built from day one so an **AI layer** (semantic search, Q&A with citations, MCP/API for agents) can be added later without re-ingesting anything.

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
| 6 | Court rulings: CBOSA (`orzeczenia.nsa.gov.pl`), common courts (`orzeczenia.ms.gov.pl`), SN, TK, SAOS (`saos.org.pl`, has an API; check how fresh its data is) | Case law that cites acts | API (SAOS) or HTML | 2 |
| 7 | EUR-Lex / Cellar | EU law that applies in Poland | SPARQL plus REST | 3 |
| 8 | Sejm legislative process API (`api.sejm.gov.pl/sejm`), RCL (`legislacja.gov.pl`) | Bills and draft acts ("upcoming changes") | JSON API (Sejm), HTML (RCL) | 3 |

**The EUREKA tax interpretations are just as important for the MVP as the acts.** They will be the largest document set to ingest: KIS issues about 25k individual interpretations a year.

---

## 2. Architecture

```
            ┌─────────────────────── ingestion (Python workers) ───────────────────────┐
 Sejm ELI ─▶│ fetcher ─▶ raw store (S3/MinIO, immutable) ─▶ parser ─▶ normalizer ─▶ DB │
 EUREKA  ──▶│   ▲ scheduler (daily delta + periodic full re-sync)                      │
 (later) ──▶│   └ job queue (Postgres-based: Procrastinate / or Redis+Celery)          │
            └──────────────────────────────────────────────────────────────────────────┘
                                         │
                       ┌─────────────────┼──────────────────┐
                       ▼                 ▼                  ▼
                 PostgreSQL         OpenSearch          (later) pgvector
              (source of truth)  (full-text, Polish     embeddings on
                                  morphology)            chunks
                       └──────────┬──────┴──────────────────┘
                                  ▼
                        API (FastAPI, REST + OpenAPI)
                                  │
                ┌─────────────────┼─────────────────────┐
                ▼                 ▼                     ▼
          Web UI (Next.js)   Public API          (later) AI layer:
          search, reader,                         RAG Q&A, MCP server,
          version timeline                        agent tools
```

**Stack choices (recommended):**
- **Python 3.12 + FastAPI** for both ingestion and the API. One language for scraping, parsing and later AI/embeddings.
- **PostgreSQL 16** as the source of truth, with the **pgvector** extension added later so the AI layer needs no new database.
- **OpenSearch** for full-text search with the `analysis-stempel` / morfologik plugin. Polish inflection makes this essential: "podatku", "podatkiem" and "podatek" must all match. Postgres FTS has no good built-in Polish stemmer.
- **MinIO / S3** for raw downloads (PDF, HTML, JSON). We can always re-parse from these without hitting the sources again.
- **Next.js** for the frontend.
- **Docker Compose** for local development. Deploy anywhere later.

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

### 3.3 General rules for all sources
- Every source gets its own **connector** module with the same interface: `list_changed(since) → ids`, `fetch(id) → raw`, `parse(raw) → normalized`.
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
document_version  (id, document_id, valid_from, valid_to, kind: original|consolidated,
                   based_on_document_id)        -- "as of date X" reading
act_unit          (id, document_version_id, parent_id, unit_type: art|ust|pkt|lit|...,
                   number, path: 'art.15/ust.1/pkt.2', ordinal, text, html)
relation          (id, from_document_id, to_document_id, type, from_unit_id?, to_unit_id?,
                   details)                      -- amends, repeals, legal_basis, consolidates...
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
- The citation graph lets retrieval expand from a hit: interpretation → cited article → amending act.

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
- Interpretation reader: sections, cited provisions (linked), similar interpretations.
- PDF download of the original.

**Search engine details**
- An OpenSearch index per doc family (`acts`, `act_units`, `interpretations`), with a Polish analyzer, synonyms and abbreviations (CIT = podatek dochodowy od osób prawnych).
- Search at the `act_unit` level so results can jump straight to the article.

---

## 6. AI layer (Phase 4, prepared from Phase 1)

1. **Embeddings:** embed each `chunk` and store it in pgvector. Use a multilingual model that handles Polish well, and benchmark candidates on our own query set before choosing.
2. **Hybrid retrieval:** combine BM25 (OpenSearch) and vector search with reciprocal rank fusion, then a reranker. Apply a temporal filter (`valid_on = date`).
3. **RAG Q&A:** the model answers **only from retrieved sources** and gives a mandatory citation for every claim, linked to the article or interpretation. Add a "not legal advice" disclaimer. If no supporting sources are found, it refuses to answer.
4. **Agent access:** run an **MCP server** that exposes these tools: `search_law`, `get_act`, `get_article(act, path, as_of)`, `search_interpretations`, `get_interpretation`, `find_citing(article)`. Claude and other agents can then use the database directly.
5. **Enrichment jobs** (offline, using an LLM):
   - interpretation summaries and tags;
   - resolving ambiguous citations;
   - a "what changed" summary for each amending act.
6. **Evaluation:** a golden set of about 200 real questions with expected source articles. Track retrieval recall@k and the accuracy of citations in answers. Run it on every model or prompt change.

---

## 7. Phased roadmap

| Phase | Duration (1–2 devs) | Deliverables |
|-------|---------------------|--------------|
| **0. Foundations** | 1 week | Repo layout (`ingest/`, `api/`, `web/`, `infra/`), Docker Compose (Postgres, OpenSearch, MinIO), DB migrations (Alembic), CI (lint + tests), connector interface |
| **0.5 Spikes** | 3–5 days | (a) ELI API: confirm endpoints, the change feed, HTML coverage by year. (b) EUREKA: map the internal JSON API, measure volume, check terms. (c) A structure parser prototype on 20 representative acts |
| **1. Acts MVP** | 3–4 weeks | Full DU+MP backfill, structure parsing, relations, daily delta, OpenSearch index, API for search and the act reader, basic web UI |
| **1b. EUREKA MVP** | 2–3 weeks (can run in parallel) | EUREKA connector, backfill, section parsing, citation extraction and linking to articles, UI for interpretations |
| **2. Quality and case law** | 3–4 weeks | Version timeline, the "changes after consolidated text" view, alias and synonym tables, monitoring (ingest lag, failed jobs), first court source (CBOSA, since it matters most for tax) |
| **3. Coverage** | ongoing | Ministerial gazettes, voivodeship gazettes (local law), EUR-Lex, legislative process |
| **4. AI** | 4–6 weeks | Chunking (built in Phase 1), embeddings, hybrid search, RAG with citations, MCP server, evaluation harness |
| **5. Product** | ongoing | Accounts, saved searches, alerts ("notify me when act X changes or new interpretations of art. Y appear"), annotations, own consolidation |

**MVP definition of done:** a user can search "ulga na złe długi". The results include the relevant article of the VAT act (with its current status) and the related EUREKA interpretations. The user can open either one and follow the links between them. The data is at most 24h old.

---

## 8. Risks and mitigations

| Risk | Mitigation |
|------|------------|
| EUREKA has no official API; endpoints may change or block us | Keep the raw store, low-rate polite crawling, alert on parser failures, ask MF for an official export |
| Older acts are PDF scans only | OCR (Tesseract with Polish models) with a quality flag; give the original PDF priority in the UI |
| Structure parsing edge cases (annexes, tables, formulas, "art. 15a¹") | Grow a test corpus of real acts; fall back to unit-less text |
| Users mistaking our consolidation or AI answers for official text | Show disclaimers, link to the official source, show the origin of every text version |
| Scale (millions of units and chunks) | Postgres partitioning by source/year if needed; OpenSearch sharding; embeddings in batches |

---

## 9. Immediate next steps
1. Approve the stack (§2). Decide where it will be hosted.
2. Phase 0: scaffold the repo and Docker Compose.
3. Run the two spikes: the ELI API and EUREKA endpoint mapping. Record the findings in `docs/sources/`.
4. Build the ELI connector and backfill one year (e.g. DU 2024) end-to-end before scaling to all years.
