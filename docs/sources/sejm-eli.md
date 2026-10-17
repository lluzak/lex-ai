# Source: Sejm ELI API

Base URL: `https://api.sejm.gov.pl/eli`. It covers Dziennik Ustaw (`DU`) and Monitor Polski (`MP`).

## Endpoints used

| Endpoint | Used for |
|----------|----------|
| `GET /acts/{pub}/{year}` | list of acts in a year (`items[]`) |
| `GET /acts/{pub}/{year}/{pos}` | act metadata: title, type, status, dates, keywords, references |
| `GET /acts/{pub}/{year}/{pos}/text.html` | structured text (when `textHTML` is true) |
| `GET /acts/{pub}/{year}/{pos}/text.pdf` | original PDF (when `textPDF` is true) |

## To verify in the spike

The connector was written from the API documentation. The API was **not reachable** from the environment where it was written, so these points are unconfirmed:

- [ ] Field names in `internal/connectors/sejmeli/types.go`: `ELI`, `address`, `displayAddress`, `changeDate`, `textHTML`, `textPDF`, `promulgation`, `announcementDate`, `entryIntoForce`, `keywords`, `references`.
- [ ] Shape of `references`: is it a map of relation label → list of `{id, art, date}`?
- [ ] Format of `changeDate`. Delta sync depends on it.
- [ ] How the HTML text marks up superscripts (`<sup>`) and structure. Is there anything better than line-based parsing?
- [ ] Whether a change feed exists that's better than re-listing the current and previous year.
- [ ] Rate limits and terms of use.
- [ ] ELI ids of the seeded aliases in `internal/db/migrations/00002_act_aliases.sql`.

Then replace the hand-written fixtures in `testdata/sejmeli/` with captured responses for a few representative acts:

- a statute with chapters;
- a code (with §);
- a regulation;
- an amending act;
- a PDF-only act from before 2000.

## Behaviour

- **Rate limit:** one shared token bucket (`LEXAI_SOURCE_RPS`, default 3 req/s), with a descriptive User-Agent.
- **Raw storage:** every response is stored raw in the blob store and recorded in the `file` table, so a field fix only needs `lexai reparse`, never a re-fetch.
- **Delta:**
  - Runs daily (`LEXAI_DELTA_INTERVAL`).
  - Re-lists the current and previous year for DU and MP, and fetches acts whose `changeDate` is newer than the stored one.
  - Unchanged content (same hashes) stops at the fetch stage.
