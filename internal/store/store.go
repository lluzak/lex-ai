// Package store holds the Postgres queries. Functions take a DB so they work
// with both the pool and a transaction.
package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/lluzak/lex-ai/internal/blobstore"
	"github.com/lluzak/lex-ai/internal/parse/act"
)

var ErrNotFound = errors.New("not found")

type DB interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	CopyFrom(ctx context.Context, table pgx.Identifier, columns []string, src pgx.CopyFromSource) (int64, error)
}

// ---- files ----

func UpsertFile(ctx context.Context, db DB, o blobstore.Object, sourceURL string) error {
	_, err := db.Exec(ctx, `
		INSERT INTO file (sha256, bucket_key, content_type, size, compression, source_url)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (sha256) DO NOTHING`,
		o.SHA256, o.Key, o.ContentType, o.Size, o.Compression, nullStr(sourceURL))
	return err
}

func GetFile(ctx context.Context, db DB, sha string) (blobstore.Object, error) {
	o := blobstore.Object{SHA256: sha}
	err := db.QueryRow(ctx, `SELECT bucket_key, content_type, size, compression FROM file WHERE sha256 = $1`, sha).
		Scan(&o.Key, &o.ContentType, &o.Size, &o.Compression)
	if errors.Is(err, pgx.ErrNoRows) {
		return o, ErrNotFound
	}
	return o, err
}

// ---- documents ----

// ActMeta is the normalized metadata of an act from the Sejm ELI source.
type ActMeta struct {
	ExternalID      string
	Title           string
	Publisher       string
	Year            int
	Position        int
	DisplayAddress  string
	SourceAddress   string
	ActType         string
	Status          string
	IssuedAt        *time.Time
	PublishedAt     *time.Time
	InForceFrom     *time.Time
	SourceChangedAt *time.Time
	MetaSHA256      string
	TextSHA256      string // empty when the act has no text yet
	TextFormat      string // html | pdf
	PDFSHA256       string // original PDF, if any
	ContentHash     string
}

// UpsertAct inserts or updates an act. It reports changed=false when the
// stored content hash is identical, so later pipeline stages can be skipped.
func UpsertAct(ctx context.Context, db DB, m ActMeta) (id int64, changed bool, err error) {
	var prevHash *string
	err = db.QueryRow(ctx, `SELECT id, content_hash FROM document WHERE source = 'sejm_eli' AND external_id = $1`, m.ExternalID).
		Scan(&id, &prevHash)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return 0, false, err
	}
	if err == nil && prevHash != nil && *prevHash == m.ContentHash {
		// Metadata like status can change without the text changing; keep it fresh.
		_, err = db.Exec(ctx, `UPDATE document SET status = $2, source_changed_at = $3, updated_at = now() WHERE id = $1`,
			id, nullStr(m.Status), m.SourceChangedAt)
		return id, false, err
	}
	err = db.QueryRow(ctx, `
		INSERT INTO document (source, external_id, doc_type, title, eli, publisher, year, position,
		                      display_address, source_address, act_type, status, issued_at, published_at, in_force_from,
		                      source_changed_at, meta_sha256, text_sha256, text_format, pdf_sha256, content_hash)
		VALUES ('sejm_eli', $1, 'act', $2, $1, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18)
		ON CONFLICT (source, external_id) DO UPDATE SET
		  title = EXCLUDED.title, publisher = EXCLUDED.publisher, year = EXCLUDED.year,
		  position = EXCLUDED.position, display_address = EXCLUDED.display_address, source_address = EXCLUDED.source_address,
		  act_type = EXCLUDED.act_type, status = EXCLUDED.status, issued_at = EXCLUDED.issued_at,
		  published_at = EXCLUDED.published_at, in_force_from = EXCLUDED.in_force_from,
		  source_changed_at = EXCLUDED.source_changed_at, meta_sha256 = EXCLUDED.meta_sha256,
		  text_sha256 = EXCLUDED.text_sha256, text_format = EXCLUDED.text_format, pdf_sha256 = EXCLUDED.pdf_sha256,
		  content_hash = EXCLUDED.content_hash, updated_at = now()
		RETURNING id`,
		m.ExternalID, m.Title, m.Publisher, m.Year, m.Position, nullStr(m.DisplayAddress), nullStr(m.SourceAddress), nullStr(m.ActType),
		nullStr(m.Status), m.IssuedAt, m.PublishedAt, m.InForceFrom, m.SourceChangedAt,
		nullStr(m.MetaSHA256), nullStr(m.TextSHA256), nullStr(m.TextFormat), nullStr(m.PDFSHA256), m.ContentHash,
	).Scan(&id)
	return id, true, err
}

// ChangeDates returns source_changed_at of stored acts of one publisher/year,
// keyed by position, for delta detection.
func ChangeDates(ctx context.Context, db DB, publisher string, year int) (map[int]time.Time, error) {
	rows, err := db.Query(ctx, `
		SELECT position, source_changed_at FROM document
		WHERE source = 'sejm_eli' AND publisher = $1 AND year = $2 AND source_changed_at IS NOT NULL`,
		publisher, year)
	if err != nil {
		return nil, err
	}
	out := map[int]time.Time{}
	for rows.Next() {
		var pos int
		var t time.Time
		if err := rows.Scan(&pos, &t); err != nil {
			return nil, err
		}
		out[pos] = t
	}
	return out, rows.Err()
}

type Document struct {
	ID             int64
	ExternalID     string
	DocType        string
	Title          string
	ELI            string
	Publisher      string
	Year           int
	Position       int
	DisplayAddress string
	SourceAddress  string
	ActType        string
	Status         string
	PublishedAt    *time.Time
	InForceFrom    *time.Time
	MetaSHA256     string
	TextSHA256     string
	TextFormat     string
	PDFSHA256      string
	ParserVersion  int
	ParseError     string
	RenderedHTML   string
	ETag           string
}

const documentColumns = `id, external_id, doc_type, title, coalesce(eli, ''), coalesce(publisher, ''),
	coalesce(year, 0), coalesce(position, 0), coalesce(display_address, ''), coalesce(source_address, ''), coalesce(act_type, ''),
	coalesce(status, ''), published_at, in_force_from, coalesce(meta_sha256, ''), coalesce(text_sha256, ''),
	coalesce(text_format, ''), coalesce(pdf_sha256, ''), parser_version, coalesce(parse_error, ''), coalesce(rendered_html, ''),
	coalesce(etag, '')`

func scanDocument(row pgx.Row) (Document, error) {
	var d Document
	err := row.Scan(&d.ID, &d.ExternalID, &d.DocType, &d.Title, &d.ELI, &d.Publisher, &d.Year, &d.Position,
		&d.DisplayAddress, &d.SourceAddress, &d.ActType, &d.Status, &d.PublishedAt, &d.InForceFrom, &d.MetaSHA256,
		&d.TextSHA256, &d.TextFormat, &d.PDFSHA256, &d.ParserVersion, &d.ParseError, &d.RenderedHTML, &d.ETag)
	if errors.Is(err, pgx.ErrNoRows) {
		return d, ErrNotFound
	}
	return d, err
}

func GetDocument(ctx context.Context, db DB, id int64) (Document, error) {
	return scanDocument(db.QueryRow(ctx, `SELECT `+documentColumns+` FROM document WHERE id = $1`, id))
}

func GetDocumentByELI(ctx context.Context, db DB, eli string) (Document, error) {
	return scanDocument(db.QueryRow(ctx, `SELECT `+documentColumns+` FROM document WHERE eli = $1 AND doc_type = 'act'`, eli))
}

// DocumentIDs returns ids of documents of a source, for bulk re-parse/re-render.
func DocumentIDs(ctx context.Context, db DB, source string) ([]int64, error) {
	rows, err := db.Query(ctx, `SELECT id FROM document WHERE source = $1 ORDER BY id`, source)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[int64])
}

func SetParsed(ctx context.Context, db DB, id int64, version int, parseErr string) error {
	_, err := db.Exec(ctx, `UPDATE document SET parser_version = $2, parse_error = $3, updated_at = now() WHERE id = $1`,
		id, version, nullStr(parseErr))
	return err
}

func SetRendered(ctx context.Context, db DB, id int64, html, etag string) error {
	_, err := db.Exec(ctx, `UPDATE document SET rendered_html = $2, etag = $3, rendered_at = now() WHERE id = $1`, id, html, etag)
	return err
}

// ---- units ----

type StoredUnit struct {
	ID       int64
	ParentID *int64
	act.Unit
}

// ReplaceUnits swaps the unit tree of a document in one COPY. Ids are
// allocated up front so parent links can be written in the same batch.
func ReplaceUnits(ctx context.Context, db DB, docID int64, units []act.Unit) error {
	if _, err := db.Exec(ctx, `DELETE FROM act_unit WHERE document_id = $1`, docID); err != nil {
		return err
	}
	if len(units) == 0 {
		return nil
	}
	rows, err := db.Query(ctx, `SELECT nextval('act_unit_id_seq') FROM generate_series(1, $1)`, len(units))
	if err != nil {
		return err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return err
	}
	src := make([][]any, len(units))
	for i, u := range units {
		var parent *int64
		if u.Parent >= 0 {
			parent = &ids[u.Parent]
		}
		src[i] = []any{ids[i], docID, parent, i, u.Depth, u.Type, u.Number, u.Path, nullStr(u.Heading), u.Text}
	}
	_, err = db.CopyFrom(ctx, pgx.Identifier{"act_unit"},
		[]string{"id", "document_id", "parent_id", "ordinal", "depth", "unit_type", "number", "path", "heading", "text"},
		pgx.CopyFromRows(src))
	if err != nil {
		return fmt.Errorf("copy units: %w", err)
	}
	return nil
}

func ListUnits(ctx context.Context, db DB, docID int64) ([]StoredUnit, error) {
	rows, err := db.Query(ctx, `
		SELECT id, parent_id, depth, unit_type, number, path, coalesce(heading, ''), text
		FROM act_unit WHERE document_id = $1 ORDER BY ordinal`, docID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StoredUnit
	for rows.Next() {
		var u StoredUnit
		if err := rows.Scan(&u.ID, &u.ParentID, &u.Depth, &u.Type, &u.Number, &u.Path, &u.Heading, &u.Text); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// ---- relations and keywords ----

type Relation struct {
	ToExternalID string
	Type         string
	Details      map[string]string
}

func ReplaceRelations(ctx context.Context, db DB, docID int64, rels []Relation) error {
	if _, err := db.Exec(ctx, `DELETE FROM relation WHERE from_document_id = $1`, docID); err != nil {
		return err
	}
	for _, r := range rels {
		if _, err := db.Exec(ctx, `INSERT INTO relation (from_document_id, to_external_id, type, details) VALUES ($1, $2, $3, $4)`,
			docID, r.ToExternalID, r.Type, r.Details); err != nil {
			return err
		}
	}
	return nil
}

// RelatedDocument is a relation joined with its target, if already ingested.
type RelatedDocument struct {
	Type        string
	Direction   string // "out": this act → target; "in": other act → this act
	ExternalID  string
	Title       string // empty when the target isn't ingested yet
	DisplayAddr string
}

func ListRelations(ctx context.Context, db DB, docID int64, externalID string) ([]RelatedDocument, error) {
	rows, err := db.Query(ctx, `
		SELECT r.type, 'out', r.to_external_id, coalesce(d.title, ''), coalesce(d.display_address, '')
		FROM relation r LEFT JOIN document d ON d.source = 'sejm_eli' AND d.external_id = r.to_external_id
		WHERE r.from_document_id = $1
		UNION ALL
		SELECT r.type, 'in', d.external_id, d.title, coalesce(d.display_address, '')
		FROM relation r JOIN document d ON d.id = r.from_document_id
		WHERE r.to_external_id = $2
		ORDER BY 1, 2, 3`, docID, externalID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RelatedDocument
	for rows.Next() {
		var r RelatedDocument
		if err := rows.Scan(&r.Type, &r.Direction, &r.ExternalID, &r.Title, &r.DisplayAddr); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func ReplaceKeywords(ctx context.Context, db DB, docID int64, keywords []string) error {
	if _, err := db.Exec(ctx, `DELETE FROM document_keyword WHERE document_id = $1`, docID); err != nil {
		return err
	}
	if len(keywords) == 0 {
		return nil
	}
	_, err := db.Exec(ctx, `
		WITH k AS (
		  INSERT INTO keyword (name) SELECT DISTINCT unnest($2::text[])
		  ON CONFLICT (name) DO UPDATE SET name = EXCLUDED.name
		  RETURNING id
		)
		INSERT INTO document_keyword (document_id, keyword_id) SELECT $1, id FROM k
		ON CONFLICT DO NOTHING`, docID, keywords)
	return err
}

func ListKeywords(ctx context.Context, db DB, docID int64) ([]string, error) {
	rows, err := db.Query(ctx, `
		SELECT k.name FROM keyword k JOIN document_keyword dk ON dk.keyword_id = k.id
		WHERE dk.document_id = $1 ORDER BY k.name`, docID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// ---- aliases ----

func LoadAliases(ctx context.Context, db DB) (map[string]string, error) {
	rows, err := db.Query(ctx, `SELECT alias, eli FROM act_alias`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var a, e string
		if err := rows.Scan(&a, &e); err != nil {
			return nil, err
		}
		out[a] = e
	}
	return out, rows.Err()
}

func nullStr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// FormatDate prints a date the Polish way ("2 stycznia 2024 r.").
func FormatDate(t *time.Time) string {
	if t == nil {
		return ""
	}
	months := [...]string{"stycznia", "lutego", "marca", "kwietnia", "maja", "czerwca",
		"lipca", "sierpnia", "września", "października", "listopada", "grudnia"}
	return fmt.Sprintf("%d %s %d r.", t.Day(), months[t.Month()-1], t.Year())
}
