// Package jobs implements the ingestion pipeline as River jobs:
//
//	sejmeli_delta → sejmeli_list_year → sejmeli_fetch_act → parse_act → render_document
//
// Each stage is idempotent and enqueues the next one in the same transaction
// as its database writes, so a crash can never lose or duplicate work.
package jobs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/lluzak/lex-ai/internal/blobstore"
	"github.com/lluzak/lex-ai/internal/connectors/sejmeli"
	"github.com/lluzak/lex-ai/internal/parse/act"
	"github.com/lluzak/lex-ai/internal/parse/htmltext"
	"github.com/lluzak/lex-ai/internal/pdftext"
	"github.com/lluzak/lex-ai/internal/render"
	"github.com/lluzak/lex-ai/internal/store"
)

// Enqueuer is the part of river.Client the pipeline needs; tests use a fake.
type Enqueuer interface {
	InsertTx(ctx context.Context, tx pgx.Tx, args river.JobArgs, opts *river.InsertOpts) (*rivertype.JobInsertResult, error)
	InsertMany(ctx context.Context, params []river.InsertManyParams) ([]*rivertype.JobInsertResult, error)
}

type Pipeline struct {
	Pool         *pgxpool.Pool
	Blobs        blobstore.Store
	ELI          *sejmeli.Client
	Enqueue      Enqueuer
	FilesBaseURL string           // public R2 domain for original PDFs; empty = don't link
	OnRendered   func(eli string) // cache invalidation hook
	Log          *slog.Logger
	Now          func() time.Time
}

func (p *Pipeline) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

// ---- stage 0: delta ----

// Delta re-lists the current and previous year of each publisher. Acts whose
// changeDate moved are re-fetched; everything else is skipped.
func (p *Pipeline) Delta(ctx context.Context) error {
	y := p.now().Year()
	var params []river.InsertManyParams
	for _, pub := range []string{"DU", "MP"} {
		for _, year := range []int{y - 1, y} {
			params = append(params, river.InsertManyParams{Args: ListYearArgs{Publisher: pub, Year: year, Run: p.now().Format("2006-01-02")}})
		}
	}
	_, err := p.Enqueue.InsertMany(ctx, params)
	return err
}

// ---- stage 1: list a year ----

// ListYear enqueues a fetch for every act of a year that is new or changed.
func (p *Pipeline) ListYear(ctx context.Context, publisher string, year int, force bool) (int, error) {
	list, _, err := p.ELI.ListActs(ctx, publisher, year)
	if err != nil {
		return 0, err
	}
	known, err := store.ChangeDates(ctx, p.Pool, publisher, year)
	if err != nil {
		return 0, err
	}
	var params []river.InsertManyParams
	for _, item := range list.Items {
		changed, _ := sejmeli.Date(item.ChangeDate)
		if prev, ok := known[item.Pos]; ok && !force && !changed.After(prev) {
			continue
		}
		params = append(params, river.InsertManyParams{Args: FetchActArgs{
			Publisher: publisher, Year: year, Pos: item.Pos, ChangeDate: item.ChangeDate, Force: force,
		}})
	}
	for start := 0; start < len(params); start += 500 {
		end := min(start+500, len(params))
		if _, err := p.Enqueue.InsertMany(ctx, params[start:end]); err != nil {
			return 0, err
		}
	}
	p.log().Info("listed year", "publisher", publisher, "year", year, "acts", len(list.Items), "enqueued", len(params))
	return len(params), nil
}

// ---- stage 2: fetch one act ----

// FetchAct downloads metadata, text and the original PDF, stores them in the
// blob store, upserts the document and enqueues parsing if anything changed.
func (p *Pipeline) FetchAct(ctx context.Context, publisher string, year, pos int, force bool) (int64, error) {
	eliPath := "/acts/" + sejmeli.ELI(publisher, year, pos)
	rawMeta, err := p.ELI.Act(ctx, publisher, year, pos)
	if err != nil {
		return 0, err
	}
	a, err := sejmeli.ParseAct(rawMeta)
	if err != nil {
		return 0, fmt.Errorf("decode act %s: %w", eliPath, err)
	}
	meta, err := blobstore.PutContent(ctx, p.Blobs, rawMeta, "application/json", true)
	if err != nil {
		return 0, err
	}

	type stored struct {
		obj blobstore.Object
		url string
	}
	var files []stored
	files = append(files, stored{meta, p.ELI.URL(eliPath)})

	var text, pdf *blobstore.Object
	if a.TextHTML {
		body, err := p.ELI.TextHTML(ctx, publisher, year, pos)
		if err != nil && !errors.Is(err, sejmeli.ErrNotFound) {
			return 0, err
		}
		if err == nil {
			o, err := blobstore.PutContent(ctx, p.Blobs, body, "text/html", true)
			if err != nil {
				return 0, err
			}
			text = &o
			files = append(files, stored{o, p.ELI.URL(eliPath + "/text.html")})
		}
	}
	if a.TextPDF {
		body, err := p.ELI.TextPDF(ctx, publisher, year, pos)
		if err != nil && !errors.Is(err, sejmeli.ErrNotFound) {
			return 0, err
		}
		if err == nil {
			o, err := blobstore.PutContent(ctx, p.Blobs, body, "application/pdf", false)
			if err != nil {
				return 0, err
			}
			pdf = &o
			files = append(files, stored{o, p.ELI.URL(eliPath + "/text.pdf")})
		}
	}

	m := store.ActMeta{
		ExternalID:     a.ExternalID(),
		Title:          a.Title,
		Publisher:      publisher,
		Year:           year,
		Position:       pos,
		DisplayAddress: a.DisplayAddress,
		SourceAddress:  a.Address,
		ActType:        a.Type,
		Status:         a.Status,
		IssuedAt:       datePtr(a.Promulgation),
		PublishedAt:    datePtr(a.AnnouncementDate),
		InForceFrom:    datePtr(firstNonEmpty(a.EntryIntoForce, a.ValidFrom)),
		MetaSHA256:     meta.SHA256,
	}
	if m.ExternalID == "" || m.ExternalID == "/0/0" {
		m.ExternalID = sejmeli.ELI(publisher, year, pos)
	}
	if t, ok := sejmeli.Date(a.ChangeDate); ok {
		m.SourceChangedAt = &t
	}
	hash := []string{meta.SHA256}
	switch {
	case text != nil:
		m.TextSHA256, m.TextFormat = text.SHA256, "html"
	case pdf != nil:
		m.TextSHA256, m.TextFormat = pdf.SHA256, "pdf"
	}
	if pdf != nil {
		m.PDFSHA256 = pdf.SHA256
	}
	hash = append(hash, m.TextSHA256, m.PDFSHA256)
	m.ContentHash = blobstore.Hash([]byte(strings.Join(hash, ":")))

	var id int64
	err = pgx.BeginFunc(ctx, p.Pool, func(tx pgx.Tx) error {
		for _, f := range files {
			if err := store.UpsertFile(ctx, tx, f.obj, f.url); err != nil {
				return err
			}
		}
		var changed bool
		var err error
		id, changed, err = store.UpsertAct(ctx, tx, m)
		if err != nil {
			return err
		}
		if changed || force {
			_, err = p.Enqueue.InsertTx(ctx, tx, ParseActArgs{DocumentID: id}, nil)
		}
		return err
	})
	return id, err
}

// ---- stage 3: parse ----

// ParseAct extracts text, splits it into units and records relations and keywords.
func (p *Pipeline) ParseAct(ctx context.Context, docID int64) error {
	doc, err := store.GetDocument(ctx, p.Pool, docID)
	if err != nil {
		return err
	}
	var units []act.Unit
	var parseErr string
	if doc.TextSHA256 == "" {
		parseErr = "no text available from source"
	} else {
		lines, err := p.textLines(ctx, doc)
		switch {
		case errors.Is(err, pdftext.ErrUnavailable):
			return err // retry later on a worker that has pdftotext
		case err != nil:
			parseErr = err.Error()
		case len(lines) == 0:
			parseErr = "empty text (scanned PDF? needs OCR)"
		default:
			units = act.Parse(lines).Units
		}
	}

	var rels []store.Relation
	var keywords []string
	if doc.MetaSHA256 != "" {
		raw, err := p.blob(ctx, doc.MetaSHA256)
		if err != nil {
			return err
		}
		a, err := sejmeli.ParseAct(raw)
		if err != nil {
			return err
		}
		keywords = a.Keywords
		for label, refs := range a.References {
			for _, r := range refs {
				target, ok := sejmeli.AddressToELI(r.ID)
				if !ok {
					continue
				}
				details := map[string]string{}
				if r.Art != "" {
					details["art"] = r.Art
				}
				if r.Date != "" {
					details["date"] = r.Date
				}
				rels = append(rels, store.Relation{ToExternalID: target, Type: label, Details: details})
			}
		}
	}

	return pgx.BeginFunc(ctx, p.Pool, func(tx pgx.Tx) error {
		if err := store.ReplaceUnits(ctx, tx, docID, units); err != nil {
			return err
		}
		if err := store.ReplaceRelations(ctx, tx, docID, rels); err != nil {
			return err
		}
		if err := store.ReplaceKeywords(ctx, tx, docID, keywords); err != nil {
			return err
		}
		if err := store.SetParsed(ctx, tx, docID, act.Version, parseErr); err != nil {
			return err
		}
		_, err := p.Enqueue.InsertTx(ctx, tx, RenderArgs{DocumentID: docID}, nil)
		return err
	})
}

func (p *Pipeline) textLines(ctx context.Context, doc store.Document) ([]string, error) {
	body, err := p.blob(ctx, doc.TextSHA256)
	if err != nil {
		return nil, err
	}
	if doc.TextFormat == "pdf" {
		text, err := pdftext.Extract(ctx, body)
		if err != nil {
			return nil, err
		}
		return pdftext.StripFurniture(htmltext.SplitLines(text)), nil
	}
	return htmltext.Lines(body)
}

func (p *Pipeline) blob(ctx context.Context, sha string) ([]byte, error) {
	f, err := store.GetFile(ctx, p.Pool, sha)
	if err != nil {
		return nil, fmt.Errorf("file %s: %w", sha, err)
	}
	return blobstore.GetContent(ctx, p.Blobs, f.Key, f.Compression)
}

// ---- stage 4: render ----

func (p *Pipeline) Render(ctx context.Context, docID int64) error {
	doc, err := store.GetDocument(ctx, p.Pool, docID)
	if err != nil {
		return err
	}
	units, err := store.ListUnits(ctx, p.Pool, docID)
	if err != nil {
		return err
	}
	keywords, err := store.ListKeywords(ctx, p.Pool, docID)
	if err != nil {
		return err
	}
	page := render.ActPage{Doc: doc, Units: units, Keywords: keywords, SourceURL: ISAPURL(doc)}
	if p.FilesBaseURL != "" && doc.PDFSHA256 != "" {
		if f, err := store.GetFile(ctx, p.Pool, doc.PDFSHA256); err == nil {
			page.PDFURL = p.FilesBaseURL + "/" + f.Key
		}
	}
	html, etag, err := render.Act(page)
	if err != nil {
		return err
	}
	if err := store.SetRendered(ctx, p.Pool, docID, html, etag); err != nil {
		return err
	}
	if p.OnRendered != nil {
		p.OnRendered(doc.ELI)
	}
	return nil
}

// ISAPURL links to the official ISAP page of an act.
func ISAPURL(d store.Document) string {
	if d.SourceAddress == "" {
		return "https://isap.sejm.gov.pl/"
	}
	return "https://isap.sejm.gov.pl/isap.nsf/DocDetails.xsp?id=" + d.SourceAddress
}

func (p *Pipeline) log() *slog.Logger {
	if p.Log != nil {
		return p.Log
	}
	return slog.Default()
}

func datePtr(s string) *time.Time {
	if t, ok := sejmeli.Date(s); ok {
		return &t
	}
	return nil
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}
