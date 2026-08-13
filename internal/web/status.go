package web

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"gpb/internal/auth"
)

type statusView struct {
	State          auth.State
	Healthy        bool
	LastWarmup     string
	LastSuccess    string
	LastError      string
	CookieCount    int
	UserAgent      string
	UsingNoSandbox bool
	KeepaliveEvery string
	ReauthRunning  bool
	Verifying      bool
}

func (s *Server) handleWarmup(w http.ResponseWriter, r *http.Request) {
	_, err := s.auth.Warmup(r.Context())

	switch {
	case err == nil:
		http.Redirect(w, r, "/reauth?notice=Session+refreshed.", http.StatusSeeOther)
	case errors.Is(err, auth.ErrProfileBusy):
		s.renderReauth(w, r, http.StatusConflict, "A browser login is in progress; finish it first.")
	case errors.Is(err, auth.ErrAuthRequired):
		s.renderReauth(w, r, http.StatusOK, "Google needs an interactive login — start one below.")
	default:
		s.renderReauth(w, r, http.StatusBadGateway, "Warmup failed: "+err.Error())
	}
}

func (s *Server) statusView() statusView {
	status := s.auth.Status()
	return statusView{
		State:          status.State,
		Healthy:        status.State == auth.StateOK,
		LastWarmup:     humanTime(status.LastWarmup),
		LastSuccess:    humanTime(status.LastSuccess),
		LastError:      status.LastError,
		CookieCount:    status.CookieCount,
		UserAgent:      status.UserAgent,
		UsingNoSandbox: status.UsingNoSandbox,
		KeepaliveEvery: humanDuration(s.cfg.Schedule.KeepaliveInterval.Duration),
		ReauthRunning:  s.reauth.Running(),
		Verifying:      status.Warming,
	}
}

func humanTime(at time.Time) string {
	if at.IsZero() {
		return "never"
	}
	return at.Local().Format("2006-01-02 15:04:05")
}

// humanClock is for a moment that is usually minutes old and read as "since …". The clock alone
// is enough while it means today; once it does not, "since 23:40" reads as last night however
// many days ago it really was, so the day goes back in.
func humanClock(at time.Time) string {
	if at.IsZero() {
		return "never"
	}

	local := at.Local()
	if !sameDay(local, time.Now()) {
		return local.Format("2006-01-02 15:04")
	}
	return local.Format("15:04")
}

func sameDay(one, other time.Time) bool {
	return one.Year() == other.Year() && one.YearDay() == other.YearDay()
}

// humanDate is the grid's form: a cell has room for the day, not the second.
func humanDate(at time.Time) string {
	if at.IsZero() {
		return "no date"
	}
	return at.Local().Format("2006-01-02")
}

// humanDuration writes a span the way config.toml spells it — "12h", not Go's "12h0m0s". The
// settings page round-trips this value through a text field, so what it shows has to be what a
// person would have typed there.
func humanDuration(span time.Duration) string {
	written := span.String()
	if strings.HasSuffix(written, "m0s") {
		written = strings.TrimSuffix(written, "0s")
	}
	if strings.HasSuffix(written, "h0m") {
		written = strings.TrimSuffix(written, "0m")
	}
	return written
}

// humanBytes uses power-of-ten units, matching what a file manager and Google both report,
// so a size shown here can be compared with one shown there without arithmetic.
func humanBytes(bytes int64) string {
	if bytes < 1000 {
		return fmt.Sprintf("%d B", bytes)
	}

	value, exponent := float64(bytes), 0
	for value >= 1000 && exponent < len(byteUnits)-1 {
		value /= 1000
		exponent++
	}
	return fmt.Sprintf("%.1f %cB", value, byteUnits[exponent])
}

const byteUnits = " kMGT"

// humanCount groups thousands. The counts on the album page run to five figures, where 44164
// has to be read a digit at a time and 44,164 does not.
func humanCount(n int) string {
	digits := strconv.Itoa(n)

	var grouped strings.Builder
	for position, digit := range digits {
		if position > 0 && (len(digits)-position)%3 == 0 {
			grouped.WriteByte(',')
		}
		grouped.WriteRune(digit)
	}
	return grouped.String()
}

// quantity keeps a noun in step with its number, so a list holding one album stops saying
// "1 albums".
func quantity(n int, noun string) string {
	if n == 1 {
		return humanCount(n) + " " + noun
	}
	return humanCount(n) + " " + noun + "s"
}
