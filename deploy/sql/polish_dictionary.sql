-- Switch the lexai_pl text search configuration to real Polish stemming.
-- Prerequisite: polish.dict, polish.affix and polish.stop are present in
-- $(pg_config --sharedir)/tsearch_data (see docs/search.md).
--
-- Run with: psql "$DATABASE_URL" -f deploy/sql/polish_dictionary.sql

BEGIN;

CREATE TEXT SEARCH DICTIONARY polish_ispell (
  TEMPLATE  = ispell,
  DictFile  = polish,
  AffFile   = polish,
  StopWords = polish
);

-- Polish words go through the ispell dictionary first; unknown words fall
-- back to simple (lower-cased) so nothing becomes unsearchable.
ALTER TEXT SEARCH CONFIGURATION lexai_pl
  ALTER MAPPING FOR asciiword, asciihword, hword_asciipart, word, hword, hword_part
  WITH polish_ispell, simple;

COMMIT;

-- Stored tsvector columns are computed on write, so recompute them once.
-- On a large database run these in a maintenance window.
UPDATE document SET title = title;
UPDATE act_unit SET text = text;
VACUUM ANALYZE document;
VACUUM ANALYZE act_unit;
