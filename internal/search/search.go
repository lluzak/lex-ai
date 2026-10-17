// Package search defines the search engine interface and its Postgres
// full-text implementation. Keeping the interface small lets us swap in
// ParadeDB pg_search later without touching handlers.
package search

import (
	"context"
	"fmt"
	"html"
	"html/template"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Query struct {
	Text      string
	DocType   string // "act", ... ; empty = all
	Publisher string // "DU", "MP"; empty = all
	YearFrom  int
	YearTo    int
	Limit     int
	Offset    int
}

type Hit struct {
	DocumentID     int64
	ELI            string
	Title          string
	DisplayAddress string
	ActType        string
	Status         string
	Year           int
	Path           string        // best matching unit, e.g. "art.15/ust.1"; empty if the title matched
	Snippet        template.HTML // escaped text with <mark> around matches
	Score          float64
}

type Engine interface {
	Search(ctx context.Context, q Query) ([]Hit, error)
}

type Postgres struct {
	Pool    *pgxpool.Pool
	Timeout time.Duration
}

const (
	selStart = "\x01"
	selStop  = "\x02"
)

func (p *Postgres) Search(ctx context.Context, q Query) ([]Hit, error) {
	if strings.TrimSpace(q.Text) == "" {
		return nil, nil
	}
	if q.Limit <= 0 || q.Limit > 100 {
		q.Limit = 20
	}
	timeout := p.Timeout
	if timeout == 0 {
		timeout = 3 * time.Second
	}

	var hits []Hit
	err := pgx.BeginFunc(ctx, p.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, fmt.Sprintf("SET LOCAL statement_timeout = %d", timeout.Milliseconds())); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `
			WITH q AS (SELECT websearch_to_tsquery('lexai_pl', $1) AS q),
			unit_hits AS (
			  SELECT u.id, u.document_id, ts_rank_cd(u.search_vector, q.q) AS rank
			  FROM act_unit u, q
			  WHERE u.search_vector @@ q.q
			  ORDER BY rank DESC
			  LIMIT 2000
			),
			best AS (
			  SELECT DISTINCT ON (document_id) document_id, id, rank
			  FROM unit_hits ORDER BY document_id, rank DESC
			),
			title_hits AS (
			  SELECT d.id AS document_id, ts_rank_cd(d.search_vector, q.q) * 4 AS rank
			  FROM document d, q WHERE d.search_vector @@ q.q
			  LIMIT 2000
			),
			cand AS (
			  SELECT document_id, max(rank) AS rank FROM (
			    SELECT document_id, rank FROM best UNION ALL SELECT document_id, rank FROM title_hits
			  ) x GROUP BY document_id
			)
			SELECT d.id, coalesce(d.eli, ''), d.title, coalesce(d.display_address, ''),
			       coalesce(d.act_type, ''), coalesce(d.status, ''), coalesce(d.year, 0),
			       coalesce(u.path, ''),
			       coalesce(ts_headline('lexai_pl', u.text, q.q,
			         'MaxFragments=1, MaxWords=35, MinWords=12, StartSel=`+selStart+`, StopSel=`+selStop+`'), ''),
			       c.rank
			FROM cand c
			JOIN document d ON d.id = c.document_id
			LEFT JOIN best b ON b.document_id = d.id
			LEFT JOIN act_unit u ON u.id = b.id
			CROSS JOIN q
			WHERE ($2 = '' OR d.doc_type = $2)
			  AND ($3 = '' OR d.publisher = $3)
			  AND ($4 = 0 OR d.year >= $4)
			  AND ($5 = 0 OR d.year <= $5)
			ORDER BY c.rank DESC, d.year DESC NULLS LAST
			LIMIT $6 OFFSET $7`,
			q.Text, q.DocType, q.Publisher, q.YearFrom, q.YearTo, q.Limit, q.Offset)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var h Hit
			var snippet string
			if err := rows.Scan(&h.DocumentID, &h.ELI, &h.Title, &h.DisplayAddress, &h.ActType, &h.Status,
				&h.Year, &h.Path, &snippet, &h.Score); err != nil {
				return err
			}
			h.Snippet = markSnippet(snippet)
			hits = append(hits, h)
		}
		return rows.Err()
	})
	return hits, err
}

// markSnippet escapes the headline and only then turns the private marker
// characters into <mark> tags, so document text can never inject HTML.
func markSnippet(s string) template.HTML {
	s = html.EscapeString(s)
	s = strings.ReplaceAll(s, selStart, "<mark>")
	s = strings.ReplaceAll(s, selStop, "</mark>")
	return template.HTML(s)
}
