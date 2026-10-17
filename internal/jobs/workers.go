package jobs

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

const (
	QueueFetch   = "fetch"   // network-bound, limited by per-host rate limits
	QueueProcess = "process" // CPU-bound: parse, render
)

// Jobs are unique while waiting or running, so re-listing a year or a
// re-render request never piles up duplicates. Completed jobs don't block a
// new run with the same args.
var pendingUnique = river.UniqueOpts{
	ByArgs: true,
	ByState: []rivertype.JobState{
		rivertype.JobStateAvailable, rivertype.JobStatePending, rivertype.JobStateRunning,
		rivertype.JobStateScheduled, rivertype.JobStateRetryable,
	},
}

// ---- args ----

type DeltaArgs struct{}

func (DeltaArgs) Kind() string { return "sejmeli_delta" }
func (DeltaArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: QueueFetch, UniqueOpts: pendingUnique}
}

type ListYearArgs struct {
	Publisher string `json:"publisher"`
	Year      int    `json:"year"`
	Force     bool   `json:"force,omitempty"`
	Run       string `json:"run,omitempty"` // distinguishes scheduled runs for uniqueness
}

func (ListYearArgs) Kind() string { return "sejmeli_list_year" }
func (ListYearArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: QueueFetch, UniqueOpts: pendingUnique}
}

type FetchActArgs struct {
	Publisher  string `json:"publisher"`
	Year       int    `json:"year"`
	Pos        int    `json:"pos"`
	ChangeDate string `json:"change_date,omitempty"`
	Force      bool   `json:"force,omitempty"`
}

func (FetchActArgs) Kind() string { return "sejmeli_fetch_act" }
func (FetchActArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: QueueFetch, UniqueOpts: pendingUnique}
}

type ParseActArgs struct {
	DocumentID int64 `json:"document_id"`
}

func (ParseActArgs) Kind() string { return "parse_act" }
func (ParseActArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: QueueProcess, UniqueOpts: pendingUnique}
}

type RenderArgs struct {
	DocumentID int64 `json:"document_id"`
}

func (RenderArgs) Kind() string { return "render_document" }
func (RenderArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: QueueProcess, UniqueOpts: pendingUnique}
}

// ---- workers ----

type deltaWorker struct {
	river.WorkerDefaults[DeltaArgs]
	p *Pipeline
}

func (w *deltaWorker) Work(ctx context.Context, _ *river.Job[DeltaArgs]) error {
	return w.p.Delta(ctx)
}

type listYearWorker struct {
	river.WorkerDefaults[ListYearArgs]
	p *Pipeline
}

func (w *listYearWorker) Work(ctx context.Context, job *river.Job[ListYearArgs]) error {
	_, err := w.p.ListYear(ctx, job.Args.Publisher, job.Args.Year, job.Args.Force)
	return err
}

func (w *listYearWorker) Timeout(*river.Job[ListYearArgs]) time.Duration { return 5 * time.Minute }

type fetchActWorker struct {
	river.WorkerDefaults[FetchActArgs]
	p *Pipeline
}

func (w *fetchActWorker) Work(ctx context.Context, job *river.Job[FetchActArgs]) error {
	_, err := w.p.FetchAct(ctx, job.Args.Publisher, job.Args.Year, job.Args.Pos, job.Args.Force)
	return err
}

func (w *fetchActWorker) Timeout(*river.Job[FetchActArgs]) time.Duration { return 10 * time.Minute }

type parseActWorker struct {
	river.WorkerDefaults[ParseActArgs]
	p *Pipeline
}

func (w *parseActWorker) Work(ctx context.Context, job *river.Job[ParseActArgs]) error {
	return w.p.ParseAct(ctx, job.Args.DocumentID)
}

type renderWorker struct {
	river.WorkerDefaults[RenderArgs]
	p *Pipeline
}

func (w *renderWorker) Work(ctx context.Context, job *river.Job[RenderArgs]) error {
	return w.p.Render(ctx, job.Args.DocumentID)
}

// Workers registers all pipeline workers.
func Workers(p *Pipeline) *river.Workers {
	w := river.NewWorkers()
	river.AddWorker(w, &deltaWorker{p: p})
	river.AddWorker(w, &listYearWorker{p: p})
	river.AddWorker(w, &fetchActWorker{p: p})
	river.AddWorker(w, &parseActWorker{p: p})
	river.AddWorker(w, &renderWorker{p: p})
	return w
}

// PeriodicJobs schedules the nightly delta.
func PeriodicJobs(interval time.Duration) []*river.PeriodicJob {
	return []*river.PeriodicJob{
		river.NewPeriodicJob(river.PeriodicInterval(interval),
			func() (river.JobArgs, *river.InsertOpts) { return DeltaArgs{}, nil },
			&river.PeriodicJobOpts{ID: "sejmeli_delta"}),
	}
}

// Queues sizes the queues: few fetchers (the bottleneck is the source's rate
// limit, not us) and more CPU workers for parse/render.
func Queues(processWorkers int) map[string]river.QueueConfig {
	if processWorkers < 1 {
		processWorkers = 1
	}
	return map[string]river.QueueConfig{
		river.QueueDefault: {MaxWorkers: 1},
		QueueFetch:         {MaxWorkers: 4},
		QueueProcess:       {MaxWorkers: processWorkers},
	}
}

// Compile-time check that river.Client satisfies Enqueuer.
var _ Enqueuer = (*river.Client[pgx.Tx])(nil)
