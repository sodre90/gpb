package web

import (
	"cmp"
	"log"
	"net/http"
	"strconv"
	"strings"

	"gpb/internal/store"
	"gpb/internal/syncer"
)

// historyDepth is how far back the Activity page reads. Enough to answer "has this been failing
// all week", short enough to be one screen and one query.
const historyDepth = 50

type runsView struct {
	Now     *nowCard
	History []historyRow
	Running bool
}

// nowCard is the run in flight. Its counts come from the runner's in-memory tally rather than
// the store, because the store learns them only when the run ends.
type nowCard struct {
	Activity   string
	Listed     int
	Downloaded int
	Failed     int
	Owed       int
	Percent    string
	Albums     []albumProgress
	StartedAt  string
}

// albumProgress is one album something is being fetched from at this moment. Its own tally comes
// from the store rather than the run: the run knows what it is downloading now, not how much of
// that album was already brought down on some previous night.
type albumProgress struct {
	Title   string
	Done    int
	Known   int
	Percent string
	Files   []fileProgress
}

// fileProgress is one file on its way down. Sized is false until the content host has said how
// long the file is, which is the only moment a percentage becomes possible — up to then the card
// shows how much has arrived instead of guessing at a fraction.
type fileProgress struct {
	Filename string
	Percent  int
	Sized    bool
	Written  string
}

// unnamedFile stands in for a listing that carried no filename. The media key is Google's own
// identifier and would say nothing to the person reading the card.
const unnamedFile = "a photo"

// historyRow is a finished run — or one that never got to finish. Interrupted is not an outcome
// the store records: it is the absence of one, which is why the row has to be told whether
// anything is running before it can name what it is looking at.
type historyRow struct {
	StartedAt   string
	Outcome     store.Outcome
	Finished    bool
	StillGoing  bool
	Interrupted bool
	Listed      int
	Downloaded  int
	Failed      int
	Size        string
	Error       string
}

// runRow is the last run as the Overview shows it: the same three states as a history row, in
// one sentence. Rendering all three as a finished run produced a verdict-shaped blank —
// "Last run 22:57:13: , 0 downloaded." — that read as a backup which had achieved nothing.
//
// The counts are deliberately absent from the unfinished sentences. Only FinishRun writes them
// to the store, so mid-run they are zero; the live ones belong to the Now card.
type runRow struct {
	Outcome    store.Outcome
	Finished   bool
	StillGoing bool
	StartedAt  string
	Downloaded int
	Failed     int
	Error      string
}

func (s *Server) handleRuns(w http.ResponseWriter, r *http.Request) {
	s.renderRuns(w, r, http.StatusOK, "")
}

func (s *Server) renderRuns(w http.ResponseWriter, r *http.Request, code int, problem string) {
	view, err := s.runsView()
	if err != nil {
		log.Printf("web: building the run history: %v", err)
		http.Error(w, "the run history is unavailable", http.StatusInternalServerError)
		return
	}

	data := s.page(r, "Activity")
	if problem != "" {
		data.Error = problem
	}
	data.Data = view
	render(w, code, "runs", data)
}

func (s *Server) nowCard() *nowCard {
	activity := s.runs.Activity()
	if activity == "" {
		return nil
	}

	progress := s.runs.Progress()
	card := &nowCard{
		Activity:   activity,
		Listed:     progress.Listed,
		Downloaded: progress.Downloaded,
		Failed:     progress.Failed,
		Owed:       progress.Owed,
		Percent:    percentOf(progress.Downloaded, progress.Owed),
		Albums:     s.inFlightAlbums(progress.Items),
	}

	// The started time comes from the run the store opened, which exists only for a sync. An
	// album listing or a refresh runs without one, and says so by leaving the line off — as does
	// a sync still warming up a browser, which has not opened its row yet.
	if run, live := s.liveRun(progress.RunID); live {
		card.StartedAt = humanTime(run.StartedAt)
	}
	return card
}

// liveRun finds the row the run in flight is writing to. Naming it is the whole difference
// between a run that is working and one the daemon was killed under: both are rows with no
// finish time, and reading "still going" from whatever else happens to be busy let an album
// listing resurrect a run that ended yesterday — and lend it its own start time, so the page
// reported a backup nineteen hours in.
func (s *Server) liveRun(runID int64) (store.SyncRun, bool) {
	if runID == 0 {
		return store.SyncRun{}, false
	}

	runs, err := s.store.RecentRuns(1)
	if err != nil || len(runs) == 0 || runs[0].ID != runID {
		return store.SyncRun{}, false
	}
	return runs[0], true
}

// inFlightAlbums groups what is being downloaded under the album it came from, keeping both the
// albums and the files within them in the order the run started them. Three workers make this a
// list of at most three, which is why an album's tally is looked up per render rather than kept.
func (s *Server) inFlightAlbums(items []syncer.InFlight) []albumProgress {
	var albums []albumProgress
	positions := map[string]int{}

	for _, item := range items {
		at, grouped := positions[item.AlbumID]
		if !grouped {
			at = len(albums)
			positions[item.AlbumID] = at
			albums = append(albums, s.albumProgressFor(item.AlbumID))
		}
		albums[at].Files = append(albums[at].Files, fileProgressFor(item))
	}
	return albums
}

func (s *Server) albumProgressFor(albumID string) albumProgress {
	album, err := s.store.Album(albumID)
	if err != nil {
		log.Printf("web: naming the album a download came from: %v", err)
		return albumProgress{}
	}

	stats, err := s.store.AlbumStats(albumID)
	if err != nil {
		log.Printf("web: summarising the album a download came from: %v", err)
		return albumProgress{Title: album.Title}
	}

	return albumProgress{
		Title:   album.Title,
		Done:    stats.Done,
		Known:   stats.Known,
		Percent: percentOf(stats.Done, stats.Known),
	}
}

func fileProgressFor(item syncer.InFlight) fileProgress {
	return fileProgress{
		Filename: cmp.Or(item.Filename, unnamedFile),
		Percent:  filePercent(item.Written, item.Total),
		Sized:    item.Total > 0,
		Written:  humanBytes(item.Written),
	}
}

// filePercent is a whole number, unlike the percentages counted in items: a tenth of a percent of
// one photo is a few kilobytes, and this figure is redrawn every second. It rounds down, so a
// file reads 100% only once its last byte is in.
func filePercent(written, total int64) int {
	if total <= 0 {
		return 0
	}
	return int(min(written*100/total, 100))
}

func (s *Server) runsView() (runsView, error) {
	runs, err := s.store.RecentRuns(historyDepth)
	if err != nil {
		return runsView{}, err
	}

	view := runsView{Now: s.nowCard()}
	view.Running = view.Now != nil
	liveRunID := s.runs.Progress().RunID
	for _, run := range runs {
		view.History = append(view.History, historyRowFor(run, liveRunID))
	}
	return view, nil
}

func historyRowFor(run store.SyncRun, liveRunID int64) historyRow {
	finished := !run.FinishedAt.IsZero()
	stillGoing := !finished && run.ID == liveRunID
	row := historyRow{
		StartedAt:   humanTime(run.StartedAt),
		Outcome:     run.Outcome,
		Finished:    finished,
		StillGoing:  stillGoing,
		Interrupted: !finished && !stillGoing,
		Listed:      run.Listed,
		Downloaded:  run.Downloaded,
		Failed:      run.Failed,
		Error:       firstLine(run.Error),
	}
	if run.Bytes > 0 {
		row.Size = humanBytes(run.Bytes)
	}
	return row
}

// firstLine keeps a run's error to one line in the table. Some carry a whole Google URL and a
// stack of wrapped contexts, which would make one bad night the tallest thing on the page.
func firstLine(message string) string {
	line, _, _ := strings.Cut(message, "\n")
	if len(line) > 200 {
		return line[:200] + "…"
	}
	return line
}

// lastRun needs to be told which run is live, because the row cannot say: an unfinished run is
// one still working if it is the one the runner is writing to, and one the daemon was killed
// under if it is not.
func (s *Server) lastRun(liveRunID int64) (*runRow, error) {
	runs, err := s.store.RecentRuns(1)
	if err != nil || len(runs) == 0 {
		return nil, err
	}

	run := runs[0]
	finished := !run.FinishedAt.IsZero()
	return &runRow{
		Outcome:    run.Outcome,
		Finished:   finished,
		StillGoing: !finished && run.ID == liveRunID,
		StartedAt:  humanTime(run.StartedAt),
		Downloaded: run.Downloaded,
		Failed:     run.Failed,
		Error:      run.Error,
	}, nil
}

// trimZero drops a trailing ".0" so a completed backup reads "100%" rather than "100.0%".
func trimZero(value float64) string {
	formatted := strconv.FormatFloat(value, 'f', 1, 64)
	return strings.TrimSuffix(formatted, ".0")
}
