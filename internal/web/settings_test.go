package web

import (
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gpb/internal/config"
	"gpb/internal/homeassistant"
)

func validSettingsForm() url.Values {
	return url.Values{
		"sync_at":             {"04:15"},
		"keepalive_interval":  {"6h"},
		"download_workers":    {"5"},
		"requests_per_second": {"3.5"},
		"min_free":            {"20.0 GB"},
		"thumb_cache_max":     {"1.0 GB"},
		"notify_command":      {""},
	}
}

func TestTheSettingsPageShowsTheValuesFromConfigToml(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()

	cfg, err := config.Load(server.cfg.DataDir())
	if err != nil {
		t.Fatalf("reading the config: %v", err)
	}

	body := get(handler, "/settings", login(t, handler)).Body.String()
	for _, want := range []string{
		cfg.Schedule.SyncAt,
		humanDuration(cfg.Schedule.KeepaliveInterval.Duration),
		humanBytes(cfg.Thumbs.CacheMaxBytes),
		cfg.PhotosDir,
		cfg.Web.Listen,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the settings page is missing %q; body was:\n%s", want, body)
		}
	}
}

// The field is round-tripped: what it shows is what gets saved back. Go's "12h0m0s" for a
// config.toml that says "12h" overflowed the input, so the page read "12h0m0" and offered to
// save something nobody wrote.
func TestTheKeepaliveFieldHoldsWhatConfigTomlSpells(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()

	body := get(handler, "/settings", login(t, handler)).Body.String()
	if !strings.Contains(body, `name="keepalive_interval" value="12h"`) {
		t.Errorf("the keepalive field does not hold a plain 12h; body was:\n%s", body)
	}
}

func TestHumanDurationDropsOnlyTheZeroUnits(t *testing.T) {
	cases := map[time.Duration]string{
		12 * time.Hour:               "12h",
		90 * time.Minute:             "1h30m",
		30 * time.Second:             "30s",
		10 * time.Second:             "10s",
		time.Minute + 30*time.Second: "1m30s",
		time.Hour + 30*time.Second:   "1h0m30s",
	}

	for span, want := range cases {
		if got := humanDuration(span); got != want {
			t.Errorf("humanDuration(%s) = %q, want %q", span, got, want)
		}
	}
}

func TestAValidPostWritesTheSettingsToConfigToml(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()
	cookie := login(t, handler)

	form := validSettingsForm()
	form.Set("notify_command", "/usr/local/bin/notify")

	recorder := postForm(handler, "/settings", form, cookie)
	if recorder.Code != http.StatusSeeOther {
		t.Fatalf("saving valid settings returned %d, want 303", recorder.Code)
	}

	reloaded, err := config.Load(server.cfg.DataDir())
	if err != nil {
		t.Fatalf("reading the config back: %v", err)
	}
	if reloaded.Schedule.SyncAt != "04:15" {
		t.Errorf("sync_at is %q, want 04:15", reloaded.Schedule.SyncAt)
	}
	if reloaded.Schedule.KeepaliveInterval.Duration != 6*time.Hour {
		t.Errorf("keepalive_interval is %s, want 6h", reloaded.Schedule.KeepaliveInterval.Duration)
	}
	if reloaded.Limits.DownloadWorkers != 5 {
		t.Errorf("download_workers is %d, want 5", reloaded.Limits.DownloadWorkers)
	}
	if reloaded.Limits.RequestsPerSecond != 3.5 {
		t.Errorf("requests_per_second is %v, want 3.5", reloaded.Limits.RequestsPerSecond)
	}
	if reloaded.Notify.Command != "/usr/local/bin/notify" {
		t.Errorf("notify_command is %q, want /usr/local/bin/notify", reloaded.Notify.Command)
	}
}

// A config the daemon would refuse to load must never be written, or the next restart fails.
func TestAnUnparsableSyncTimeIsRefusedAndChangesNothingOnDisk(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()
	cookie := login(t, handler)

	before, err := config.Load(server.cfg.DataDir())
	if err != nil {
		t.Fatalf("reading the config: %v", err)
	}

	form := validSettingsForm()
	form.Set("sync_at", "half past three")

	recorder := postForm(handler, "/settings", form, cookie)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("an unparsable sync time returned %d, want 400", recorder.Code)
	}

	after, err := config.Load(server.cfg.DataDir())
	if err != nil {
		t.Fatalf("reading the config back: %v", err)
	}
	if after.Schedule.SyncAt != before.Schedule.SyncAt {
		t.Errorf("sync_at changed to %q despite being rejected", after.Schedule.SyncAt)
	}
}

// ParseDuration alone accepts a sub-minute interval; config.Validate is what actually rejects it.
func TestAKeepaliveIntervalBelowAMinuteIsRefusedAndChangesNothingOnDisk(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()
	cookie := login(t, handler)

	before, err := config.Load(server.cfg.DataDir())
	if err != nil {
		t.Fatalf("reading the config: %v", err)
	}

	form := validSettingsForm()
	form.Set("keepalive_interval", "30s")

	recorder := postForm(handler, "/settings", form, cookie)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("a sub-minute keepalive interval returned %d, want 400", recorder.Code)
	}

	after, err := config.Load(server.cfg.DataDir())
	if err != nil {
		t.Fatalf("reading the config back: %v", err)
	}
	if after.Schedule.KeepaliveInterval.Duration != before.Schedule.KeepaliveInterval.Duration {
		t.Errorf("keepalive_interval changed to %s despite being rejected",
			after.Schedule.KeepaliveInterval.Duration)
	}
}

func TestTheThumbnailCacheCapRoundTripsThroughItsDisplayUnits(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()
	cookie := login(t, handler)

	form := validSettingsForm()
	form.Set("thumb_cache_max", "2.0 GB")

	if recorder := postForm(handler, "/settings", form, cookie); recorder.Code != http.StatusSeeOther {
		t.Fatalf("saving returned %d, want 303", recorder.Code)
	}

	reloaded, err := config.Load(server.cfg.DataDir())
	if err != nil {
		t.Fatalf("reading the config back: %v", err)
	}
	if reloaded.Thumbs.CacheMaxBytes != 2_000_000_000 {
		t.Errorf("the cache cap is %d bytes, want 2,000,000,000", reloaded.Thumbs.CacheMaxBytes)
	}
}

func TestTheFreeSpaceFloorRoundTripsThroughItsDisplayUnits(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()
	cookie := login(t, handler)

	form := validSettingsForm()
	form.Set("min_free", "50.0 GB")

	if recorder := postForm(handler, "/settings", form, cookie); recorder.Code != http.StatusSeeOther {
		t.Fatalf("saving returned %d, want 303", recorder.Code)
	}

	reloaded, err := config.Load(server.cfg.DataDir())
	if err != nil {
		t.Fatalf("reading the config back: %v", err)
	}
	if reloaded.Limits.MinFreeBytes != 50_000_000_000 {
		t.Errorf("the floor is %d bytes, want 50,000,000,000", reloaded.Limits.MinFreeBytes)
	}
	if shown := get(handler, "/settings", cookie).Body.String(); !strings.Contains(shown, "50.0 GB") {
		t.Error("the settings page does not show the floor it just saved")
	}
}

// Nobody sets a disk reserve in bytes, so a bare number in that field is gigabytes — typing 40
// and getting 40 bytes is a floor that is off in all but name.
func TestABareNumberOfFreeSpaceIsGigabytes(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()
	cookie := login(t, handler)

	for _, typed := range []struct {
		value string
		want  int64
	}{
		{"40", 40_000_000_000},
		{"40 GB", 40_000_000_000},
		{"500 MB", 500_000_000},
		{"1024 B", 1024},
	} {
		form := validSettingsForm()
		form.Set("min_free", typed.value)

		if recorder := postForm(handler, "/settings", form, cookie); recorder.Code != http.StatusSeeOther {
			t.Fatalf("saving a floor of %q returned %d, want 303", typed.value, recorder.Code)
		}
		reloaded, err := config.Load(server.cfg.DataDir())
		if err != nil {
			t.Fatalf("reading the config back: %v", err)
		}
		if reloaded.Limits.MinFreeBytes != typed.want {
			t.Errorf("a floor of %q was saved as %d bytes, want %d",
				typed.value, reloaded.Limits.MinFreeBytes, typed.want)
		}
	}
}

// The thumbnail cap is named after the bytes it holds and has always been edited in them, so the
// unit a bare number takes has to be the field's own rather than the page's.
func TestABareThumbnailCapIsStillBytes(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()
	cookie := login(t, handler)

	form := validSettingsForm()
	form.Set("thumb_cache_max", "1500000000")

	if recorder := postForm(handler, "/settings", form, cookie); recorder.Code != http.StatusSeeOther {
		t.Fatalf("saving returned %d, want 303", recorder.Code)
	}

	reloaded, err := config.Load(server.cfg.DataDir())
	if err != nil {
		t.Fatalf("reading the config back: %v", err)
	}
	if reloaded.Thumbs.CacheMaxBytes != 1_500_000_000 {
		t.Errorf("the cache cap is %d bytes, want 1,500,000,000", reloaded.Thumbs.CacheMaxBytes)
	}
}

// Zero is the documented off switch, so the form has to be able to write it. Every other size on
// this page would be nonsense at zero, and a validation rule written for those would take the
// switch away.
func TestAFreeSpaceFloorOfZeroIsAcceptedAsTurningTheCheckOff(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()
	cookie := login(t, handler)

	form := validSettingsForm()
	form.Set("min_free", "0")

	if recorder := postForm(handler, "/settings", form, cookie); recorder.Code != http.StatusSeeOther {
		t.Fatalf("saving a floor of zero returned %d, want 303", recorder.Code)
	}

	reloaded, err := config.Load(server.cfg.DataDir())
	if err != nil {
		t.Fatalf("reading the config back: %v", err)
	}
	if reloaded.Limits.MinFreeBytes != 0 {
		t.Errorf("the floor is %d bytes after being turned off", reloaded.Limits.MinFreeBytes)
	}
}

func TestASizeThatIsNotASizeSaysWhichSettingItMeant(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()
	cookie := login(t, handler)

	form := validSettingsForm()
	form.Set("min_free", "lots")

	recorder := postForm(handler, "/settings", form, cookie)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("an unreadable floor returned %d, want 400", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), "the free space to keep") {
		t.Error("the page does not say which of its two size fields was rejected")
	}
}

// The broker password is a credential for a machine on the user's own network, and a page that
// renders it puts it in the page source, the back button and any proxy log on the way.
func TestTheBrokerPasswordIsNeverRenderedBackIntoThePage(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()
	cookie := login(t, handler)

	form := validSettingsForm()
	form.Set("mqtt_broker", "192.168.1.10")
	form.Set("mqtt_username", "gpb")
	form.Set("mqtt_password", "a-broker-secret")

	if recorder := postForm(handler, "/settings", form, cookie); recorder.Code != http.StatusSeeOther {
		t.Fatalf("saving returned %d, want 303", recorder.Code)
	}

	body := get(handler, "/settings", cookie).Body.String()
	if strings.Contains(body, "a-broker-secret") {
		t.Error("the settings page renders the broker password")
	}
	if !strings.Contains(body, "one is set") {
		t.Error("the settings page does not say that a broker password is stored")
	}
}

// The field is always empty, so an empty field cannot mean "clear it" — that would wipe the
// password every time anything else on the page was saved.
func TestSavingWithAnEmptyPasswordFieldKeepsTheStoredOne(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()
	cookie := login(t, handler)

	form := validSettingsForm()
	form.Set("mqtt_broker", "192.168.1.10")
	form.Set("mqtt_password", "a-broker-secret")
	postForm(handler, "/settings", form, cookie)

	form.Set("mqtt_password", "")
	form.Set("sync_at", "05:00")
	postForm(handler, "/settings", form, cookie)

	kept, err := config.Load(server.cfg.DataDir())
	if err != nil {
		t.Fatalf("reading the config back: %v", err)
	}
	if kept.MQTT.Password != "a-broker-secret" {
		t.Errorf("the broker password is now %q after saving an unrelated setting", kept.MQTT.Password)
	}
	if kept.Schedule.SyncAt != "05:00" {
		t.Error("the unrelated setting was not saved")
	}

	form.Set("mqtt_password_clear", "1")
	postForm(handler, "/settings", form, cookie)

	cleared, err := config.Load(server.cfg.DataDir())
	if err != nil {
		t.Fatalf("reading the config back: %v", err)
	}
	if cleared.MQTT.Password != "" {
		t.Error("asking outright for the broker password to be forgotten did not clear it")
	}
}

// The address field is the one a whole URL gets pasted into, and a broker's own documentation
// writes the credentials into that URL. The field beside it is never rendered back; this one is
// rendered back into a value attribute, so what lands in it has to be an address and nothing else.
func TestAPasswordPastedIntoTheBrokerAddressIsMovedIntoItsOwnField(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()
	cookie := login(t, handler)

	form := validSettingsForm()
	form.Set("mqtt_broker", "mqtt://gpb:a-broker-secret@192.168.1.10")

	if recorder := postForm(handler, "/settings", form, cookie); recorder.Code != http.StatusSeeOther {
		t.Fatalf("saving returned %d, want 303", recorder.Code)
	}

	saved, err := config.Load(server.cfg.DataDir())
	if err != nil {
		t.Fatalf("reading the config back: %v", err)
	}
	switch {
	case strings.Contains(saved.MQTT.Broker, "a-broker-secret"):
		t.Errorf("the stored address is %q, which still carries the password", saved.MQTT.Broker)
	case saved.MQTT.Username != "gpb" || saved.MQTT.Password != "a-broker-secret":
		t.Errorf("the pasted credentials became %q/%q", saved.MQTT.Username, saved.MQTT.Password)
	}

	if body := get(handler, "/settings", cookie).Body.String(); strings.Contains(body, "a-broker-secret") {
		t.Error("the settings page renders back the password that was pasted into the address")
	}
}

// Every save posts every field, so a size the user never looked at is submitted back in the
// rounded form it was rendered in. Reading that back as the new value rewrites a reserve someone
// set deliberately, with a number they never typed.
func TestASizeNobodyEditedIsSavedBackByteForByte(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()
	cookie := login(t, handler)

	const awkward = int64(8_589_934_592)
	stored, err := config.Load(server.cfg.DataDir())
	if err != nil {
		t.Fatalf("reading the config: %v", err)
	}
	stored.Limits.MinFreeBytes = awkward
	if err := stored.Save(); err != nil {
		t.Fatalf("writing a size that does not round trip: %v", err)
	}

	form := validSettingsForm()
	form.Set("min_free", humanBytes(awkward))
	form.Set("notify_command", "/usr/local/bin/notify")
	if recorder := postForm(handler, "/settings", form, cookie); recorder.Code != http.StatusSeeOther {
		t.Fatalf("saving returned %d, want 303", recorder.Code)
	}

	saved, err := config.Load(server.cfg.DataDir())
	if err != nil {
		t.Fatalf("reading the config back: %v", err)
	}
	if saved.Limits.MinFreeBytes != awkward {
		t.Errorf("a reserve of %d bytes came back as %d after saving an unrelated setting",
			awkward, saved.Limits.MinFreeBytes)
	}

	form.Set("min_free", "40 GB")
	if recorder := postForm(handler, "/settings", form, cookie); recorder.Code != http.StatusSeeOther {
		t.Fatalf("saving an edited size returned %d, want 303", recorder.Code)
	}
	edited, err := config.Load(server.cfg.DataDir())
	if err != nil {
		t.Fatalf("reading the config back: %v", err)
	}
	if edited.Limits.MinFreeBytes != 40*oneGigabyte {
		t.Errorf("an edited reserve saved as %d bytes, want %d", edited.Limits.MinFreeBytes, 40*oneGigabyte)
	}
}

func TestABrokerThatIsNotAnAddressIsRefused(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()
	cookie := login(t, handler)

	form := validSettingsForm()
	form.Set("mqtt_broker", "tcp://")

	recorder := postForm(handler, "/settings", form, cookie)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("an addressless broker returned %d, want 400", recorder.Code)
	}
}

// The handler re-reads the file before writing, precisely so a password set by `gpb passwd`
// since startup survives a settings save. A regression here would log the user out permanently.
func TestSavingSettingsDoesNotDestroyThePasswordHash(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()
	cookie := login(t, handler)

	before, err := config.Load(server.cfg.DataDir())
	if err != nil {
		t.Fatalf("reading the config: %v", err)
	}
	if before.Web.PasswordHash == "" {
		t.Fatal("the test config has no password hash to protect")
	}

	if recorder := postForm(handler, "/settings", validSettingsForm(), cookie); recorder.Code != http.StatusSeeOther {
		t.Fatalf("saving settings returned %d, want 303", recorder.Code)
	}

	after, err := config.Load(server.cfg.DataDir())
	if err != nil {
		t.Fatalf("reading the config back: %v", err)
	}
	if after.Web.PasswordHash != before.Web.PasswordHash {
		t.Error("saving settings changed the password hash")
	}
}

// fakeBridge is the Home Assistant bridge as the settings page sees it: something with an
// opinion about the broker, and something that can be told to go and get a new one.
type fakeBridge struct {
	standing homeassistant.Status
	redials  int
}

func (f *fakeBridge) Status() homeassistant.Status { return f.standing }

func (f *fakeBridge) Reload() {
	f.redials++
	f.standing.Attempted = time.Now()
}

func settingsPageWithBroker(t *testing.T, standing homeassistant.Status) (*Server, http.Handler) {
	t.Helper()

	server, _ := testServer(t)
	cfg, err := config.Load(server.cfg.DataDir())
	if err != nil {
		t.Fatalf("reading the scratch config: %v", err)
	}
	cfg.MQTT.Broker = "192.168.1.10"
	if err := cfg.Save(); err != nil {
		t.Fatalf("writing a broker into the scratch config: %v", err)
	}

	server.mqtt = &fakeBridge{standing: standing}
	return server, server.Handler()
}

// Diagnosing the first real broker meant reading the daemon's journal over ssh. The reason
// belongs where the broker was typed in.
func TestTheSettingsPageSaysWhyTheBrokerWillNotHaveUs(t *testing.T) {
	_, handler := settingsPageWithBroker(t, homeassistant.Status{
		Broker: "tcp://192.168.1.10:1883",
		Detail: "not Authorized",
		Since:  time.Now(),
	})

	body := get(handler, "/settings", login(t, handler)).Body.String()
	for _, want := range []string{"Not connected", "not Authorized"} {
		if !strings.Contains(body, want) {
			t.Errorf("the settings page does not say %q; body was:\n%s", want, body)
		}
	}
}

func TestTheSettingsPageSaysWhenTheBrokerIsAnswering(t *testing.T) {
	_, handler := settingsPageWithBroker(t, homeassistant.Status{
		Broker:    "tcp://192.168.1.10:1883",
		Connected: true,
		Detail:    "publishing as gpb",
		Since:     time.Now(),
	})

	body := get(handler, "/settings", login(t, handler)).Body.String()
	if !strings.Contains(body, "Connected") || strings.Contains(body, "Not connected") {
		t.Errorf("a working broker does not read as connected; body was:\n%s", body)
	}
}

// A link that has been up for days used to read "since 23:40", which is how tonight reads. The
// broker's whole value on this page is telling a connection that is holding from one that is
// dropping and redialling, and a bare clock time hides exactly that.
func TestALongStandingBrokerConnectionSaysWhichDayItDatesFrom(t *testing.T) {
	threeDaysAgo := time.Now().Add(-3 * 24 * time.Hour)
	_, handler := settingsPageWithBroker(t, homeassistant.Status{
		Broker:    "tcp://192.168.1.10:1883",
		Connected: true,
		Detail:    "publishing as gpb",
		Since:     threeDaysAgo,
	})

	body := get(handler, "/settings", login(t, handler)).Body.String()
	if !strings.Contains(body, threeDaysAgo.Local().Format("2006-01-02")) {
		t.Errorf("a connection three days old is shown without its date; body was:\n%s", body)
	}
}

func TestAConnectionMadeTodayIsShownAsAClockTime(t *testing.T) {
	made := time.Now()
	_, handler := settingsPageWithBroker(t, homeassistant.Status{
		Broker:    "tcp://192.168.1.10:1883",
		Connected: true,
		Detail:    "publishing as gpb",
		Since:     made,
	})

	body := get(handler, "/settings", login(t, handler)).Body.String()
	if !strings.Contains(body, "since "+made.Local().Format("15:04")) {
		t.Errorf("a connection made today is not shown as a plain clock time; body was:\n%s", body)
	}
}

// Saving is the test: the settings the page was saved over are the ones the bridge is holding,
// so a correction that is not dialled is a correction nobody can see the result of.
func TestSavingNewBrokerSettingsDialsAgain(t *testing.T) {
	server, handler := settingsPageWithBroker(t, homeassistant.Status{})

	form := validSettingsForm()
	form.Set("mqtt_broker", "192.168.1.99")
	if code := postForm(handler, "/settings", form, login(t, handler)).Code; code != http.StatusSeeOther {
		t.Fatalf("saving returned %d, want 303", code)
	}

	if redials := server.mqtt.(*fakeBridge).redials; redials != 1 {
		t.Errorf("a changed broker was dialled again %d times, want once", redials)
	}
}

// A connection is worth keeping. Saving the backup time should not drop one.
func TestSavingSomethingElseLeavesTheBrokerAlone(t *testing.T) {
	server, handler := settingsPageWithBroker(t, homeassistant.Status{Connected: true})

	form := validSettingsForm()
	form.Set("mqtt_broker", "192.168.1.10")
	form.Set("sync_at", "05:45")
	if code := postForm(handler, "/settings", form, login(t, handler)).Code; code != http.StatusSeeOther {
		t.Fatalf("saving returned %d, want 303", code)
	}

	if redials := server.mqtt.(*fakeBridge).redials; redials != 0 {
		t.Errorf("an unchanged broker was dialled again %d times", redials)
	}
}

// A broker that was down when gpb last tried needs no settings changed, only asking again — and
// saving would not ask, because nothing about it has changed.
func TestTryingAgainDialsTheBrokerWithoutChangingAnything(t *testing.T) {
	server, handler := settingsPageWithBroker(t, homeassistant.Status{Detail: "connection refused"})

	recorder := postForm(handler, "/settings/broker", validSettingsForm(), login(t, handler))
	if recorder.Code != http.StatusSeeOther {
		t.Fatalf("asking the broker again returned %d, want 303", recorder.Code)
	}
	if redials := server.mqtt.(*fakeBridge).redials; redials != 1 {
		t.Errorf("the broker was dialled %d times, want once", redials)
	}

	reloaded, err := config.Load(server.cfg.DataDir())
	if err != nil {
		t.Fatalf("reading the config back: %v", err)
	}
	if reloaded.Schedule.SyncAt == "04:15" {
		t.Error("asking the broker again saved the form it was pressed from")
	}
}

// A daemon that cannot listen is a daemon whose settings page cannot be reached to correct it,
// so a certificate that will not load has to be refused here, while the page is still on screen.
func TestACertificateThatWillNotLoadIsRefusedBeforeItIsSaved(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()
	cookie := login(t, handler)

	notACertificate := filepath.Join(t.TempDir(), "cert.pem")
	if err := os.WriteFile(notACertificate, []byte("this is not a certificate\n"), 0o600); err != nil {
		t.Fatalf("writing a file that is not a certificate: %v", err)
	}

	form := validSettingsForm()
	form.Set("external_url", "https://192.168.1.10:8090")
	form.Set("tls_mode", "provided")
	form.Set("tls_cert_file", notACertificate)
	form.Set("tls_key_file", notACertificate)

	recorder := postForm(handler, "/settings", form, cookie)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("a certificate that will not load was answered with %d, want 400", recorder.Code)
	}

	saved, err := config.Load(server.cfg.DataDir())
	if err != nil {
		t.Fatalf("reading the config back: %v", err)
	}
	if saved.Web.TLS.Enabled() {
		t.Errorf("the refused certificate was written anyway: the daemon would not start with %q", saved.Web.TLS.Mode)
	}
}

// The certificate is made for one address and no other, and inside a container that address can
// only come from this setting. Making one against an empty address would produce a certificate
// no browser accepts, and no amount of clicking through the warning would fix it.
func TestSelfSigningNeedsTheAddressItIsReachedAtFirst(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()
	cookie := login(t, handler)

	form := validSettingsForm()
	form.Set("external_url", "")
	form.Set("tls_mode", "self-signed")

	recorder := postForm(handler, "/settings", form, cookie)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("self-signing with no address was answered with %d, want 400", recorder.Code)
	}
	if body := recorder.Body.String(); !strings.Contains(body, "the address this page is reached at") {
		t.Error("the refusal does not say what to fill in")
	}

	form.Set("external_url", "https://192.168.1.10:8090")
	if recorder := postForm(handler, "/settings", form, cookie); recorder.Code != http.StatusSeeOther {
		t.Fatalf("self-signing with an address was answered with %d, want 303", recorder.Code)
	}
	saved, err := config.Load(server.cfg.DataDir())
	if err != nil {
		t.Fatalf("reading the config back: %v", err)
	}
	if saved.Web.TLS.Mode != config.TLSSelfSigned {
		t.Errorf("the mode was saved as %q, want self-signed", saved.Web.TLS.Mode)
	}
}

// The address is what the sign-in link is built from. Moving the UI to https and leaving the
// address saying http sends the user to a page that does not answer, and the only place that can
// be noticed is here.
func TestThePageSaysWhenTheAddressAndTheSchemeDisagree(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()
	cookie := login(t, handler)

	cfg, err := config.Load(server.cfg.DataDir())
	if err != nil {
		t.Fatalf("reading the config: %v", err)
	}
	cfg.Web.ExternalURL = "http://192.168.1.10:8090"
	cfg.Web.TLS = config.TLS{Mode: config.TLSSelfSigned}
	if err := cfg.Save(); err != nil {
		t.Fatalf("saving: %v", err)
	}

	body := get(handler, "/settings", cookie).Body.String()
	if !strings.Contains(body, "will lead nowhere") {
		t.Error("the page serves https and announces http, and says nothing about it")
	}

	cfg.Web.ExternalURL = "https://192.168.1.10:8090"
	if err := cfg.Save(); err != nil {
		t.Fatalf("saving: %v", err)
	}
	if body := get(handler, "/settings", cookie).Body.String(); strings.Contains(body, "will lead nowhere") {
		t.Error("the address and the scheme agree and the page complains anyway")
	}
}

// Every save posts every field, so a page saved for some other reason must not turn encryption
// off underneath the user.
func TestSavingSomethingElseLeavesEncryptionAlone(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()
	cookie := login(t, handler)

	form := validSettingsForm()
	form.Set("external_url", "https://192.168.1.10:8090")
	form.Set("tls_mode", "self-signed")
	if recorder := postForm(handler, "/settings", form, cookie); recorder.Code != http.StatusSeeOther {
		t.Fatalf("turning encryption on: %d", recorder.Code)
	}

	form.Set("notify_command", "/usr/local/bin/tell-me")
	if recorder := postForm(handler, "/settings", form, cookie); recorder.Code != http.StatusSeeOther {
		t.Fatalf("saving the hook command: %d", recorder.Code)
	}

	saved, err := config.Load(server.cfg.DataDir())
	if err != nil {
		t.Fatalf("reading the config back: %v", err)
	}
	if saved.Web.TLS.Mode != config.TLSSelfSigned {
		t.Errorf("saving the hook command left encryption %q", saved.Web.TLS.Mode)
	}
	if saved.Notify.Command != "/usr/local/bin/tell-me" {
		t.Errorf("the hook command was saved as %q", saved.Notify.Command)
	}
}

// Two settings hold one fact: the mode below, and the scheme inside the address the sign-in links
// are built from. Choosing encryption and then being told off for the address still saying http is
// asking the user to keep the second copy by hand.
func TestChoosingEncryptionMovesTheAddressOntoTheSchemeItWillAnswerOn(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()
	cookie := login(t, handler)

	form := validSettingsForm()
	form.Set("external_url", "http://192.168.1.10:8090")
	form.Set("tls_mode", "self-signed")

	recorder := postForm(handler, "/settings", form, cookie)
	if recorder.Code != http.StatusSeeOther {
		t.Fatalf("turning encryption on was answered with %d, want 303", recorder.Code)
	}
	if notice := recorder.Header().Get("Location"); !strings.Contains(notice, "the+address+now+reads") {
		t.Errorf("the address was rewritten without saying so: %q", notice)
	}

	saved, err := config.Load(server.cfg.DataDir())
	if err != nil {
		t.Fatalf("reading the config back: %v", err)
	}
	if saved.Web.ExternalURL != "https://192.168.1.10:8090" {
		t.Errorf("the address reads %q, want https://192.168.1.10:8090", saved.Web.ExternalURL)
	}

	form.Set("external_url", saved.Web.ExternalURL)
	form.Set("tls_mode", "off")
	if recorder := postForm(handler, "/settings", form, cookie); recorder.Code != http.StatusSeeOther {
		t.Fatalf("turning encryption off was answered with %d, want 303", recorder.Code)
	}
	if saved, err = config.Load(server.cfg.DataDir()); err != nil {
		t.Fatalf("reading the config back: %v", err)
	}
	if saved.Web.ExternalURL != "http://192.168.1.10:8090" {
		t.Errorf("after turning encryption off the address reads %q, want http://192.168.1.10:8090",
			saved.Web.ExternalURL)
	}
}

// Only the save that changed the encryption setting touches the address. Anyone terminating TLS in
// a proxy in front of this is reached over a scheme gpb does not serve itself, and every later save
// would fight them for the field.
func TestASaveThatLeavesEncryptionAloneLeavesTheAddressAlone(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()
	cookie := login(t, handler)

	form := validSettingsForm()
	form.Set("external_url", "https://192.168.1.10:8090")
	form.Set("tls_mode", "self-signed")
	if recorder := postForm(handler, "/settings", form, cookie); recorder.Code != http.StatusSeeOther {
		t.Fatalf("turning encryption on: %d", recorder.Code)
	}

	form.Set("external_url", "http://gpb.home.lan")
	recorder := postForm(handler, "/settings", form, cookie)
	if recorder.Code != http.StatusSeeOther {
		t.Fatalf("saving an address of one's own was answered with %d, want 303", recorder.Code)
	}
	if notice := recorder.Header().Get("Location"); strings.Contains(notice, "the+address+now+reads") {
		t.Errorf("a save that changed no encryption setting claimed to rewrite the address: %q", notice)
	}

	saved, err := config.Load(server.cfg.DataDir())
	if err != nil {
		t.Fatalf("reading the config back: %v", err)
	}
	if saved.Web.ExternalURL != "http://gpb.home.lan" {
		t.Errorf("the address reads %q, want the http://gpb.home.lan that was typed", saved.Web.ExternalURL)
	}
}

// A bare "192.168.1.10:8090" parses with the host where the scheme goes, so an address written
// without one has to be left alone rather than rewritten into one that has lost its host.
func TestAnAddressWithNoSchemeIsLeftAsItIs(t *testing.T) {
	for _, address := range []string{"192.168.1.10:8090", ""} {
		if got := addressServedOver(address, "https"); got != address {
			t.Errorf("addressServedOver(%q) = %q, want it untouched", address, got)
		}
	}
	if got := addressServedOver("http://192.168.1.10:8090", "https"); got != "https://192.168.1.10:8090" {
		t.Errorf("addressServedOver on an http address = %q, want it on https", got)
	}
}
