# Search

Search runs on Postgres full-text search, using one text search configuration called `lexai_pl`. Every `tsvector` column uses it.

## Polish stemming

Polish is heavily inflected. A query for "podatek" should also match "podatku" and "podatkiem". Stock Postgres has no Polish dictionary, so the first migration creates `lexai_pl` as `simple` + `unaccent`:

- It works everywhere.
- It only matches exact word forms.
- It has no stop words, so words like "na" and "w" are matched and highlighted.

To enable real Polish stemming:

1. Get a Polish ispell/hunspell dictionary in UTF-8. Options:
   - the sjp.pl ispell dictionary;
   - the LibreOffice/hunspell `pl_PL` dictionary.

   Name the files `polish.dict` and `polish.affix`. Also add a `polish.stop` stop-word list (one word per line).
2. Put the three files in `deploy/tsearch_data/`. Then uncomment the three volume lines in `docker-compose.yml`, or copy the files to `$(pg_config --sharedir)/tsearch_data/` on a non-Docker Postgres.
3. Run `psql "$DATABASE_URL" -f deploy/sql/polish_dictionary.sql`. This creates `polish_ispell`, puts it in front of `simple` in `lexai_pl`, and recomputes the stored vectors.

Notes:
- Postgres loads an ispell dictionary once per connection, which costs time and memory. `lexai` uses a long-lived connection pool, so this cost is paid rarely. If it becomes a problem, the `shared_ispell` extension keeps one copy in shared memory.
- Check results with `SELECT to_tsvector('lexai_pl', 'podatkiem od towarów');`.

## Exact lookups

Before running a full-text search, the search box checks whether the query points at one document (`internal/lookup`):

| Query | Goes to |
|-------|---------|
| `Dz.U. 2024 poz. 1`, `Dz. U. z 2024 r. poz. 1`, `Dz.U. 1964 nr 16 poz. 93` | `/akt/DU/2024/1` |
| `M.P. 2023 poz. 5`, `MP/2023/5` | `/akt/MP/2023/5` |
| `art. 415 k.c.`, `art. 86 ust. 1 ustawy o VAT` | the act from `act_alias`, anchored at the provision |

A normal form submit redirects straight to the document. While you type (htmx), the match is shown as a "Przejdź do" link above the results.

## Performance

- Every search runs in a transaction with `statement_timeout` (3 s by default).
- Candidates are capped (2000 unit hits, 2000 title hits) before ranking.
- **Still to verify:** latency on the full corpus. Planned as a spike in PLAN.md §9. The fallback is ParadeDB `pg_search` behind the `search.Engine` interface.
