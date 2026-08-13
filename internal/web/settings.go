package web

import (
	"crypto/tls"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"gpb/internal/config"
)

// settingsView holds the file's values as text, because that is what the form round-trips. The
// durations especially: "12h" is what config.toml says and what a person writing one would type,
// and turning it into a number of minutes for the sake of an <input type="number"> would make
// the page and the file disagree about the same setting.
type settingsView struct {
	SyncAt            string
	KeepaliveInterval string
	DownloadWorkers   int
	RequestsPerSecond string
	MinFree           string
	ThumbCacheMax     string
	NotifyCommand     string
	MQTTBroker        string
	MQTTUsername      string
	MQTTHasPassword   bool
	MQTTTopicPrefix   string
	MQTTConnected     bool
	MQTTDetail        string
	MQTTSince         string
	PhotosDir         string
	Listen            string
	TLSMode           string
	TLSCertFile       string
	TLSKeyFile        string
	// TLSExpires and TLSProblem are what is true of the certificate right now, as opposed to what
	// the file asks for: a date the user can act on before it passes, or the reason there is
	// nothing to serve with.
	TLSExpires string
	TLSProblem string
	// TLSSchemeMismatch is the trap this setting has all to itself. The address in external_url
	// is what the sign-in links are built from, and a UI moved to https that still announces
	// itself as http sends the user to a page that does not answer.
	TLSSchemeMismatch bool
	ExternalURL       string
	Scheme            string
	// CanRestart is whether anything would start gpb again, and Activity what a restart would
	// interrupt: a run cut off is picked up by the next one, but the reader deserves to know
	// before rather than after.
	CanRestart bool
	Activity   string
}

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	s.renderSettings(w, r, http.StatusOK, "")
}

func (s *Server) renderSettings(w http.ResponseWriter, r *http.Request, code int, problem string) {
	cfg, err := config.Load(s.cfg.DataDir())
	if err != nil {
		log.Printf("web: reading the config for the settings page: %v", err)
		http.Error(w, "the settings are unreadable", http.StatusInternalServerError)
		return
	}

	data := s.page(r, "Settings")
	if problem != "" {
		data.Error = problem
	}
	view := settingsViewOf(cfg)
	view.MQTTConnected, view.MQTTDetail, view.MQTTSince = s.brokerStanding()
	view.Activity = s.runs.Activity()
	data.Data = view
	render(w, code, "settings", data)
}

// brokerStanding is the bridge's own account of itself, so that a refusal shows up where the
// broker was typed in rather than only in the daemon's log on the server.
func (s *Server) brokerStanding() (connected bool, detail, since string) {
	if s.mqtt == nil {
		return false, "", ""
	}

	standing := s.mqtt.Status()
	if standing.Since.IsZero() {
		return false, "not tried yet", ""
	}
	return standing.Connected, standing.Detail, humanClock(standing.Since)
}

func settingsViewOf(cfg config.Config) settingsView {
	view := settingsView{
		SyncAt:            cfg.Schedule.SyncAt,
		KeepaliveInterval: humanDuration(cfg.Schedule.KeepaliveInterval.Duration),
		DownloadWorkers:   cfg.Limits.DownloadWorkers,
		RequestsPerSecond: strconv.FormatFloat(cfg.Limits.RequestsPerSecond, 'f', -1, 64),
		MinFree:           humanBytes(cfg.Limits.MinFreeBytes),
		ThumbCacheMax:     humanBytes(cfg.Thumbs.CacheMaxBytes),
		NotifyCommand:     cfg.Notify.Command,
		MQTTBroker:        cfg.MQTT.Broker,
		MQTTUsername:      cfg.MQTT.Username,
		// The broker password is never sent back to the browser, only whether there is one. A
		// value in the HTML is a value in the page source, in the back button and in any proxy
		// log between here and there.
		MQTTHasPassword: cfg.MQTT.Password != "",
		MQTTTopicPrefix: cfg.MQTT.TopicPrefix,
		PhotosDir:       cfg.PhotosDir,
		Listen:          cfg.Web.Listen,
		TLSMode:         string(cfg.Web.TLS.Mode),
		TLSCertFile:     cfg.Web.TLS.CertFile,
		TLSKeyFile:      cfg.Web.TLS.KeyFile,
		ExternalURL:     cfg.Web.ExternalURL,
		Scheme:          cfg.Web.TLS.Scheme(),
		CanRestart:      supervised(),
	}
	view.TLSExpires, view.TLSProblem = certificateStanding(cfg)
	view.TLSSchemeMismatch = schemeMismatch(cfg)
	return view
}

// certificateStanding reports on the certificate as it is on disk. A self-signed one that has not
// been made yet is neither a date nor a problem — it is made at the next start, which is when
// this setting takes effect at all.
func certificateStanding(cfg config.Config) (expires, problem string) {
	if !cfg.Web.TLS.Enabled() {
		return "", ""
	}

	certFile := cfg.Web.TLS.CertFile
	if cfg.Web.TLS.Mode == config.TLSSelfSigned {
		if certFile, _ = selfSignedPaths(cfg); !fileExists(certFile) {
			return "", ""
		}
	}
	expiry, err := certificateExpiry(certFile)
	if err != nil {
		return "", fmt.Sprintf("the certificate at %s cannot be read: %v", certFile, err)
	}
	return expiry.Format("2 January 2006"), ""
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func schemeMismatch(cfg config.Config) bool {
	announced, err := url.Parse(cfg.Web.ExternalURL)
	if err != nil || announced.Scheme == "" {
		return false
	}
	return announced.Scheme != cfg.Web.TLS.Scheme()
}

// handleSettingsSave rewrites config.toml from the file on disk rather than from the config this
// process started with, so a password set by `gpb passwd` since startup is not written back out
// of a stale copy.
func (s *Server) handleSettingsSave(w http.ResponseWriter, r *http.Request) {
	cfg, err := config.Load(s.cfg.DataDir())
	if err != nil {
		log.Printf("web: reading the config back before saving: %v", err)
		s.renderSettings(w, r, http.StatusInternalServerError, "The settings could not be read back.")
		return
	}

	edited, err := applySettings(cfg, r)
	if err != nil {
		s.renderSettings(w, r, http.StatusBadRequest, err.Error())
		return
	}
	if err := edited.Save(); err != nil {
		log.Printf("web: saving the config: %v", err)
		s.renderSettings(w, r, http.StatusInternalServerError, "The settings could not be written.")
		return
	}
	if edited.MQTT != cfg.MQTT {
		s.redialTheBroker()
	}

	notice := "Saved. " + savedNotice
	if edited.Web.ExternalURL != strings.TrimSpace(r.FormValue("external_url")) {
		notice = fmt.Sprintf("Saved, and the address now reads %s to match. %s",
			edited.Web.ExternalURL, savedNotice)
	}
	http.Redirect(w, r, "/settings?"+url.Values{"notice": {notice}}.Encode(), http.StatusSeeOther)
}

// handleBrokerRedial is for the broker that was down rather than the settings that were wrong:
// nothing to change, nothing to save, just ask again now. Saving already re-dials, but only when
// a field changed, which leaves no way to retry the settings that are already right.
func (s *Server) handleBrokerRedial(w http.ResponseWriter, r *http.Request) {
	s.redialTheBroker()
	http.Redirect(w, r, "/settings", http.StatusSeeOther)
}

// redialTheBroker makes saving the page the test of what was typed into it: the bridge is asked
// to dial again, and this waits for its answer so that the page it redirects to already has one.
// A broker that is simply not there takes its own good time to say so, hence the deadline —
// after which the connection line is a page refresh behind rather than wrong.
func (s *Server) redialTheBroker() {
	if s.mqtt == nil {
		return
	}

	asked := time.Now()
	s.mqtt.Reload()
	for waited := time.Duration(0); waited < brokerAnswerDeadline; waited += brokerAnswerPoll {
		if s.mqtt.Status().Attempted.After(asked) {
			return
		}
		s.wait(brokerAnswerPoll)
	}
}

const (
	brokerAnswerDeadline = 5 * time.Second
	brokerAnswerPoll     = 50 * time.Millisecond
)

// savedNotice is honest about the limit of this page: the daemon reads its schedule and limits
// once, at startup. Saying "saved" alone would leave someone waiting for a 02:00 backup that is
// still set to run at 03:30 until the container is restarted. The broker is the exception — it
// is dialled again on the spot, which is why the connection line below can be believed.
const savedNotice = "The broker is dialled again at once; everything else applies when the daemon next starts."

func applySettings(cfg config.Config, r *http.Request) (config.Config, error) {
	keepalive, err := time.ParseDuration(r.FormValue("keepalive_interval"))
	if err != nil {
		return cfg, fmt.Errorf("the keepalive interval must be a duration such as 12h")
	}
	workers, err := strconv.Atoi(r.FormValue("download_workers"))
	if err != nil {
		return cfg, fmt.Errorf("the number of download workers must be a whole number")
	}
	requests, err := strconv.ParseFloat(r.FormValue("requests_per_second"), 64)
	if err != nil {
		return cfg, fmt.Errorf("requests per second must be a number such as 2.0")
	}
	minFree, err := editedBytes(r.FormValue("min_free"), cfg.Limits.MinFreeBytes, "the free space to keep", oneGigabyte)
	if err != nil {
		return cfg, err
	}
	cacheMax, err := editedBytes(r.FormValue("thumb_cache_max"), cfg.Thumbs.CacheMaxBytes, "the thumbnail cache cap", oneByte)
	if err != nil {
		return cfg, err
	}

	cfg.Schedule.SyncAt = r.FormValue("sync_at")
	cfg.Schedule.KeepaliveInterval = config.Duration{Duration: keepalive}
	cfg.Limits.DownloadWorkers = workers
	cfg.Limits.RequestsPerSecond = requests
	cfg.Limits.MinFreeBytes = minFree
	cfg.Thumbs.CacheMaxBytes = cacheMax
	cfg.Notify.Command = r.FormValue("notify_command")
	cfg.Web.ExternalURL = strings.TrimSpace(r.FormValue("external_url"))
	servedOver := cfg.Web.TLS.Scheme()
	if cfg, err = applyTLS(cfg, r); err != nil {
		return cfg, err
	}
	if cfg.Web.TLS.Scheme() != servedOver {
		cfg.Web.ExternalURL = addressServedOver(cfg.Web.ExternalURL, cfg.Web.TLS.Scheme())
	}
	cfg.MQTT.Broker = strings.TrimSpace(r.FormValue("mqtt_broker"))
	cfg.MQTT.Username = strings.TrimSpace(r.FormValue("mqtt_username"))
	cfg.MQTT.Password = editedPassword(cfg.MQTT.Password, r)
	if prefix := strings.TrimSpace(r.FormValue("mqtt_topic_prefix")); prefix != "" {
		cfg.MQTT.TopicPrefix = prefix
	}
	// After the password field rather than before it: a whole URL pasted into the address is the
	// more recent statement of what the credentials are.
	cfg.MQTT = cfg.MQTT.WithoutBrokerUserinfo()

	if err := cfg.Validate(); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// addressServedOver puts external_url on the scheme the UI is about to answer on, and is called
// only from the save that changed the encryption setting: that is the moment the user said what
// they wanted, and rewriting on every save would fight anyone who terminates TLS in a proxy in
// front of this. An address whose scheme is neither http nor https is left alone — a bare
// "192.168.1.10:8090" parses with the host as the scheme, and rewriting that would eat the host.
func addressServedOver(address, scheme string) string {
	reached, err := url.Parse(address)
	if err != nil || (reached.Scheme != "http" && reached.Scheme != "https") {
		return address
	}
	reached.Scheme = scheme
	return reached.String()
}

// applyTLS is where a certificate is tried, rather than at the next start. A daemon that will not
// listen is a daemon whose settings page cannot be reached to correct it, so anything that would
// stop it listening has to be refused here, while the page is still on screen.
func applyTLS(cfg config.Config, r *http.Request) (config.Config, error) {
	cfg.Web.TLS.Mode = config.TLSOff
	if chosen := config.TLSMode(r.FormValue("tls_mode")); chosen != "" {
		cfg.Web.TLS.Mode = chosen
	}
	cfg.Web.TLS.CertFile = strings.TrimSpace(r.FormValue("tls_cert_file"))
	cfg.Web.TLS.KeyFile = strings.TrimSpace(r.FormValue("tls_key_file"))

	switch cfg.Web.TLS.Mode {
	case config.TLSSelfSigned:
		// The certificate is made for the address in external_url and for nothing else, because
		// that is the only address this process can know it is reached at. Without one there is
		// nothing to put in it, and a certificate for the wrong name is a certificate no browser
		// will accept however many times its owner says to.
		if reached, err := url.Parse(cfg.Web.ExternalURL); err != nil || reached.Hostname() == "" {
			return cfg, fmt.Errorf("a self-signed certificate is made for one address, so fill in " +
				"the address this page is reached at first — something like https://192.168.1.10:8090")
		}
	case config.TLSProvided:
		if err := cfg.Web.TLS.Validate(); err != nil {
			return cfg, err
		}
		if _, err := tls.LoadX509KeyPair(cfg.Web.TLS.CertFile, cfg.Web.TLS.KeyFile); err != nil {
			return cfg, fmt.Errorf("that certificate and key cannot be used: %v", err)
		}
	}
	return cfg, nil
}

// editedPassword keeps the broker password that is already stored when the field comes back
// empty, because that field is always empty: it is never rendered with a value. Clearing one
// therefore has to be said outright, which is what the checkbox beside it is for.
func editedPassword(stored string, r *http.Request) string {
	if r.FormValue("mqtt_password_clear") != "" {
		return ""
	}
	if typed := r.FormValue("mqtt_password"); typed != "" {
		return typed
	}
	return stored
}

const (
	oneByte     = int64(1)
	oneGigabyte = int64(1_000_000_000)
)

// editedBytes keeps a size that came back exactly as it was rendered. humanBytes rounds to one
// decimal place, so reading its output back is lossy: 8589934592 is shown as "8.6 GB" and parses
// as 8600000000. Every save posts every field, so without this a reserve nobody had touched was
// rewritten by its own rounding the first time the notify command was changed.
func editedBytes(submitted string, stored int64, setting string, unitOfABareNumber int64) (int64, error) {
	if strings.TrimSpace(submitted) == humanBytes(stored) {
		return stored, nil
	}
	return parseBytes(submitted, setting, unitOfABareNumber)
}

// parseBytes reads back what humanBytes wrote, so a size can be edited in the units it is shown
// in. What a bare number means is the caller's to say: nobody types a disk reserve in bytes, so
// 40 in that field is 40 GB, while the thumbnail cache is capped in the bytes its own setting is
// named after. The setting names itself in case the value is rejected — two size fields on one
// page make "that is not a size" useless.
func parseBytes(value, setting string, unitOfABareNumber int64) (int64, error) {
	trimmed, unit := strings.TrimSpace(value), int64(1)
	for exponent := 1; exponent < len(byteUnits); exponent++ {
		prefix := string(byteUnits[exponent])
		unit *= 1000
		if after, found := strings.CutSuffix(trimmed, prefix+"B"); found {
			return scaleBytes(after, unit, setting)
		}
	}
	if after, found := strings.CutSuffix(trimmed, "B"); found {
		return scaleBytes(after, oneByte, setting)
	}
	return scaleBytes(trimmed, unitOfABareNumber, setting)
}

func scaleBytes(amount string, unit int64, setting string) (int64, error) {
	scaled, err := strconv.ParseFloat(strings.TrimSpace(amount), 64)
	if err != nil || scaled < 0 {
		return 0, fmt.Errorf("%s must be a size such as 40 GB", setting)
	}
	return int64(scaled * float64(unit)), nil
}
