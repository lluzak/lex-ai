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
- An OpenSearch index per doc family (`acts`, `act_units`, `interpretations`, `rulings`), with a Polish analyzer, synonyms and abbreviations (CIT = podatek dochodowy od osób prawnych).
- Search at the `act_unit` level so results can jump straight to the article.

---

## 6. AI layer (Phase 4, prepared from Phase 1)

1. **Embeddings:** embed each `chunk` and store it in pgvector. Use a multilingual model that handles Polish well, and benchmark candidates on our own query set before choosing.
2. **Hybrid retrieval:** combine BM25 (OpenSearch) and vector search with reciprocal rank fusion, then a reranker. Apply a temporal filter (`valid_on = date`).
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
| **0. Foundations** | 1 week | Repo layout (`ingest/`, `api/`, `web/`, `infra/`), Docker Compose (Postgres, OpenSearch, MinIO), DB migrations (Alembic), CI (lint + tests), connector interface |
| **0.5 Spikes** | 3–5 days | (a) ELI API: confirm endpoints, the change feed, HTML coverage by year. (b) EUREKA: map the internal JSON API, measure volume, check terms. (c) CBOSA page structure, SAOS freshness and dump format. (d) A structure parser prototype on 20 representative acts |
| **1. Acts MVP** | 3–4 weeks | Full DU+MP backfill, structure parsing, relations, daily delta, OpenSearch index, API for search and the act reader, basic web UI |
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
| Scale (millions of units and chunks) | Postgres partitioning by source/year if needed; OpenSearch sharding; embeddings in batches |

---

## 9. Immediate next steps
1. Approve the stack (§2). Decide where it will be hosted.
2. Phase 0: scaffold the repo and Docker Compose.
3. Run the two spikes: the ELI API and EUREKA endpoint mapping. Record the findings in `docs/sources/`.
4. Add a third spike: CBOSA page structure and SAOS freshness/dump format.
5. Build the ELI connector and backfill one year (e.g. DU 2024) end-to-end before scaling to all years.
