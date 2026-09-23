package web

import (
	"log"
	"net/http"

	"gpb/internal/auth"
	"gpb/internal/store"
)

// overviewView answers one question — is the backup healthy — and nothing else. Everything on
// it is a summary with somewhere to go for the detail, because the page is read at a glance and
// most of the time the answer is "yes" and the reader leaves.
type overviewView struct {
	Backup  backupCard
	LastRun *runRow
	Now     *nowCard
	Session sessionCard
	Waiting int
	Running bool
}

// backupCard is what the user has actually asked to have, counted across the account. Empty and
// Walked are carried rather than derived in the template, because the difference between "there
// is nothing to do" and "nobody has looked yet" is the whole reason the card exists, and is not
// a decision markup should be making.
type backupCard struct {
	Albums   int
	Library  bool
	Empty    bool
	Walked   bool
	Expected int
	Known    int
	Done     int
	Pending  int
	Failed   int
	Percent  string
	OnDisk   string
	Photos   string
	Videos   string
	// Free is what the pool's filesystem has left, and Cramped whether that is less than the
	// floor a run stops at. A library is far larger than the disk it is usually pointed at, and
	// the difference is only interesting before a run hits it: afterwards the run has already
	// stopped and said so. Free is empty when the filesystem could not be measured, which is not
	// worth an error on a page whose job is a glance.
	Free    string
	Cramped bool
}

func backupCardFor(set store.BackupSet) backupCard {
	return backupCard{
		Albums:   set.Albums,
		Library:  set.Library,
		Empty:    set.Empty(),
		Walked:   set.Walked(),
		Expected: set.Expected,
		Known:    set.Known,
		Done:     set.Done,
		Pending:  set.Pending,
		Failed:   set.Failed,
		Percent:  percentOf(set.Done, set.Known),
		OnDisk:   humanBytes(set.Bytes),
		Photos:   humanBytes(set.Bytes - set.VideoBytes),
		Videos:   humanBytes(set.VideoBytes),
	}
}

func (s *Server) roomLeft(card backupCard) backupCard {
	free, err := s.freeBytes(s.cfg.PhotosDir)
	if err != nil {
		log.Printf("web: measuring free space on %s: %v", s.cfg.PhotosDir, err)
		return card
	}

	card.Free = humanBytes(int64(free))
	card.Cramped = s.cfg.Limits.MinFreeBytes > 0 && free < uint64(s.cfg.Limits.MinFreeBytes)
	return card
}

func percentOf(done, total int) string {
	if total == 0 {
		return "0"
	}
	return trimZero(float64(done) * 100 / float64(total))
}

// sessionCard is the Google half of "is everything fine". It says the state and when it was
// last confirmed, and leaves the diagnostics to the page that owns them.
type sessionCard struct {
	State       auth.State
	Healthy     bool
	LastSuccess string
}

func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request) {
	view, err := s.overviewView()
	if err != nil {
		log.Printf("web: building the overview: %v", err)
		http.Error(w, "the overview is unavailable", http.StatusInternalServerError)
		return
	}

	data := s.page(r, "Overview")
	data.Data = view
	render(w, http.StatusOK, "overview", data)
}

func (s *Server) overviewView() (overviewView, error) {
	set, err := s.store.BackupSet()
	if err != nil {
		return overviewView{}, err
	}

	status := s.auth.Status()
	view := overviewView{
		Backup:  s.roomLeft(backupCardFor(set)),
		Now:     s.nowCard(),
		Waiting: s.waitingForReview(),
		Session: sessionCard{
			State:       status.State,
			Healthy:     status.State == auth.StateOK,
			LastSuccess: humanTime(status.LastSuccess),
		},
	}
	view.Running = view.Now != nil

	view.LastRun, err = s.lastRun(s.runs.Progress().RunID)
	return view, err
}
