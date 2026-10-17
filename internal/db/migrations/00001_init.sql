-- +goose Up
CREATE EXTENSION IF NOT EXISTS pg_trgm;
CREATE EXTENSION IF NOT EXISTS unaccent;

-- Text search configuration used everywhere. It starts as `simple` + unaccent
-- so it works on a stock Postgres. When the Polish ispell dictionary files are
-- installed (see docs/search.md), run deploy/sql/polish_dictionary.sql to
-- switch the mapping to real Polish stemming. Columns keep the same config
-- name, so no schema change is needed, only a reindex.
-- +goose StatementBegin
DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_ts_config WHERE cfgname = 'lexai_pl') THEN
    CREATE TEXT SEARCH CONFIGURATION lexai_pl (COPY = simple);
    ALTER TEXT SEARCH CONFIGURATION lexai_pl
      ALTER MAPPING FOR hword, hword_part, word WITH unaccent, simple;
  END IF;
END
$$;
-- +goose StatementEnd

CREATE TABLE source (
  code        text PRIMARY KEY,
  name        text NOT NULL,
  base_url    text NOT NULL
);

INSERT INTO source (code, name, base_url) VALUES
  ('sejm_eli', 'Sejm ELI API (Dziennik Ustaw, Monitor Polski)', 'https://api.sejm.gov.pl/eli'),
  ('eureka',   'EUREKA (Ministerstwo Finansów)',                'https://eureka.mf.gov.pl'),
  ('cbosa',    'CBOSA (NSA/WSA)',                               'https://orzeczenia.nsa.gov.pl'),
  ('saos',     'SAOS',                                          'https://www.saos.org.pl');

-- Raw and original files. The bytes live in the blob store (Cloudflare R2);
-- this table only records where and what they are.
CREATE TABLE file (
  sha256        text PRIMARY KEY,
  bucket_key    text NOT NULL,
  content_type  text NOT NULL,
  size          bigint NOT NULL,
  compression   text NOT NULL DEFAULT 'none',  -- none | zstd
  source_url    text,
  fetched_at    timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE document (
  id                 bigserial PRIMARY KEY,
  source             text NOT NULL REFERENCES source(code),
  external_id        text NOT NULL,             -- e.g. "DU/2024/1"
  doc_type           text NOT NULL,             -- act | interpretation_individual | ruling | ...
  title              text NOT NULL,
  eli                text,                      -- ELI id for acts, e.g. "DU/2024/1"
  publisher          text,                      -- DU | MP
  year               int,
  position           int,
  display_address    text,                      -- "Dz.U. 2024 poz. 1"
  source_address     text,                      -- ISAP address, e.g. "WDU20240000001"
  act_type           text,                      -- Ustawa | Rozporządzenie | ...
  status             text,                      -- as reported by the source
  issued_at          date,
  published_at       date,
  in_force_from      date,
  source_changed_at  timestamptz,               -- source's own change timestamp, for delta sync
  meta_sha256        text REFERENCES file(sha256),
  text_sha256        text REFERENCES file(sha256),
  text_format        text,                      -- html | pdf
  pdf_sha256         text REFERENCES file(sha256), -- original PDF, kept even when HTML text exists
  content_hash       text,                      -- hash of meta + text, to skip unchanged content
  parser_version     int NOT NULL DEFAULT 0,
  parse_error        text,
  rendered_html      text,                      -- pre-rendered page body
  etag               text,
  rendered_at        timestamptz,
  search_vector      tsvector GENERATED ALWAYS AS (
                        setweight(to_tsvector('lexai_pl', coalesce(title, '')), 'A') ||
                        setweight(to_tsvector('lexai_pl', coalesce(display_address, '')), 'A')
                      ) STORED,
  created_at         timestamptz NOT NULL DEFAULT now(),
  updated_at         timestamptz NOT NULL DEFAULT now(),
  UNIQUE (source, external_id)
);

CREATE INDEX document_eli_idx ON document (eli);
CREATE INDEX document_type_year_idx ON document (doc_type, year);
CREATE INDEX document_search_idx ON document USING gin (search_vector);
CREATE INDEX document_title_trgm_idx ON document USING gin (title gin_trgm_ops);

-- Structural units of an act: część, dział, rozdział, art., ust., §, pkt, lit., tiret.
CREATE TABLE act_unit (
  id             bigserial PRIMARY KEY,
  document_id    bigint NOT NULL REFERENCES document(id) ON DELETE CASCADE,
  parent_id      bigint REFERENCES act_unit(id) ON DELETE CASCADE,
  ordinal        int NOT NULL,                 -- reading order within the document
  depth          int NOT NULL,
  unit_type      text NOT NULL,                -- czesc | ksiega | tytul | dzial | rozdzial | oddzial | art | par | ust | pkt | lit | tiret
  number         text NOT NULL,
  path           text NOT NULL,                -- citation path, e.g. "art.15/ust.1/pkt.2"
  heading        text,
  text           text NOT NULL DEFAULT '',
  search_vector  tsvector GENERATED ALWAYS AS (
                    setweight(to_tsvector('lexai_pl', coalesce(heading, '')), 'B') ||
                    setweight(to_tsvector('lexai_pl', text), 'C')
                  ) STORED,
  UNIQUE (document_id, ordinal)
);

CREATE INDEX act_unit_path_idx ON act_unit (document_id, path);
CREATE INDEX act_unit_search_idx ON act_unit USING gin (search_vector);

-- Typed edges between documents. The target is stored by its external id so a
-- relation can be recorded before the target act has been ingested.
CREATE TABLE relation (
  id                 bigserial PRIMARY KEY,
  from_document_id   bigint NOT NULL REFERENCES document(id) ON DELETE CASCADE,
  to_external_id     text NOT NULL,
  type               text NOT NULL,            -- source label, e.g. "Akty zmienione"
  details            jsonb NOT NULL DEFAULT '{}'::jsonb
);

CREATE INDEX relation_from_idx ON relation (from_document_id);
CREATE INDEX relation_to_idx ON relation (to_external_id);

CREATE TABLE keyword (
  id    bigserial PRIMARY KEY,
  name  text NOT NULL UNIQUE
);

CREATE TABLE document_keyword (
  document_id  bigint NOT NULL REFERENCES document(id) ON DELETE CASCADE,
  keyword_id   bigint NOT NULL REFERENCES keyword(id) ON DELETE CASCADE,
  PRIMARY KEY (document_id, keyword_id)
);

-- Common names and abbreviations for acts ("k.c.", "ustawa o VAT").
CREATE TABLE act_alias (
  alias        text PRIMARY KEY,               -- normalized: lower case, no dots/extra spaces
  eli          text NOT NULL
);

-- Resumable backfill / delta position per source and scope.
CREATE TABLE source_cursor (
  source      text NOT NULL REFERENCES source(code),
  scope       text NOT NULL,                   -- e.g. "DU/2024"
  cursor      text NOT NULL,
  updated_at  timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (source, scope)
);

-- +goose Down
DROP TABLE source_cursor;
DROP TABLE act_alias;
DROP TABLE document_keyword;
DROP TABLE keyword;
DROP TABLE relation;
DROP TABLE act_unit;
DROP TABLE document;
DROP TABLE file;
DROP TABLE source;
DROP TEXT SEARCH CONFIGURATION IF EXISTS lexai_pl;
