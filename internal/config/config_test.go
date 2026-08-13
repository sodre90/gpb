package config

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestPasswordRoundTrip(t *testing.T) {
	hash, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatalf("hashing: %v", err)
	}

	ok, err := VerifyPassword(hash, "correct horse battery staple")
	if err != nil || !ok {
		t.Fatalf("verify of the right password: ok=%v err=%v", ok, err)
	}

	ok, err = VerifyPassword(hash, "correct horse battery stapl")
	if err != nil {
		t.Fatalf("verify of a wrong password errored: %v", err)
	}
	if ok {
		t.Fatal("a wrong password verified")
	}
}

func TestHashesAreSalted(t *testing.T) {
	first, _ := HashPassword("same password")
	second, _ := HashPassword("same password")
	if first == second {
		t.Fatal("two hashes of the same password are identical, so the salt is not random")
	}
}

func TestVerifyRejectsMalformedHashes(t *testing.T) {
	for _, encoded := range []string{
		"",
		"not-a-phc-string",
		"$argon2i$v=19$m=65536,t=3,p=1$c2FsdA$aGFzaA",
		"$argon2id$v=13$m=65536,t=3,p=1$c2FsdA$aGFzaA",
		"$argon2id$v=19$m=65536,t=3,p=1$!!!!$aGFzaA",
	} {
		if _, err := VerifyPassword(encoded, "whatever"); err == nil {
			t.Errorf("VerifyPassword(%q) accepted a malformed hash", encoded)
		}
	}
}

func TestVerifyHonoursStoredParameters(t *testing.T) {
	hash, _ := HashPassword("parameterised")
	if !strings.Contains(hash, "m=65536,t=3,p=1") {
		t.Fatalf("stored parameters missing from %q", hash)
	}
}

func TestLoadWritesDefaultsOnFirstRun(t *testing.T) {
	dir := t.TempDir()

	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("first load: %v", err)
	}
	if cfg.Web.Listen != ":8080" || cfg.Limits.DownloadWorkers != 3 {
		t.Fatalf("defaults not applied: %+v", cfg)
	}

	reloaded, err := Load(dir)
	if err != nil {
		t.Fatalf("second load: %v", err)
	}
	if reloaded.Schedule.KeepaliveInterval.Duration != 12*time.Hour {
		t.Fatalf("durations did not survive the round trip: %v", reloaded.Schedule.KeepaliveInterval)
	}
	if reloaded.DataDir() != dir {
		t.Fatalf("data dir lost on reload: %q", reloaded.DataDir())
	}
}

func TestSetPasswordPersists(t *testing.T) {
	dir := t.TempDir()
	cfg, _ := Load(dir)

	if cfg.HasPassword() {
		t.Fatal("a fresh config already has a password")
	}
	if err := cfg.SetPassword("hunter2hunter2"); err != nil {
		t.Fatalf("SetPassword: %v", err)
	}

	reloaded, err := Load(dir)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	ok, err := VerifyPassword(reloaded.Web.PasswordHash, "hunter2hunter2")
	if err != nil || !ok {
		t.Fatalf("password did not survive the round trip: ok=%v err=%v", ok, err)
	}
}

func TestValidateRejectsUnusableSchedules(t *testing.T) {
	cfg := Defaults()
	cfg.Schedule.SyncAt = "half past three"
	if err := cfg.Validate(); err == nil {
		t.Fatal("a non-HH:MM sync_at was accepted")
	}

	cfg = Defaults()
	cfg.Schedule.KeepaliveInterval = Duration{30 * time.Second}
	if err := cfg.Validate(); err == nil {
		t.Fatal("a sub-minute keepalive interval was accepted")
	}

	cfg = Defaults()
	cfg.Limits.RequestsPerSecond = 0
	if err := cfg.Validate(); err == nil {
		t.Fatal("a zero request rate was accepted")
	}

	cfg = Defaults()
	cfg.Limits.MinFreeBytes = -1
	if err := cfg.Validate(); err == nil {
		t.Fatal("a negative free-space floor was accepted")
	}
}

// Zero is the off switch, so it has to survive being written and read back. A round trip that
// quietly restored the default would leave someone who turned the floor off with it still on.
func TestAFreeSpaceFloorOfZeroSurvivesTheRoundTrip(t *testing.T) {
	dir := t.TempDir()
	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("first load: %v", err)
	}
	if cfg.Limits.MinFreeBytes == 0 {
		t.Fatal("a fresh config keeps no free space in reserve")
	}

	cfg.Limits.MinFreeBytes = 0
	if err := cfg.Save(); err != nil {
		t.Fatalf("saving: %v", err)
	}

	reloaded, err := Load(dir)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.Limits.MinFreeBytes != 0 {
		t.Errorf("the floor came back as %d bytes after being turned off", reloaded.Limits.MinFreeBytes)
	}
}

// Zero means no floor at all, so an absent key must not read as zero. Load decodes over the
// defaults, which is what keeps a config written before this setting existed from silently
// losing its reserve.
func TestAConfigWrittenBeforeThisSettingExistedKeepsItsFloor(t *testing.T) {
	dir := t.TempDir()
	older := "photos_dir = \"/photos\"\n\n[limits]\ndownload_workers = 4\nrequests_per_second = 2.0\n"
	if err := os.WriteFile(Path(dir), []byte(older), 0o600); err != nil {
		t.Fatalf("writing an older config: %v", err)
	}

	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("loading an older config: %v", err)
	}
	if cfg.Limits.MinFreeBytes != Defaults().Limits.MinFreeBytes {
		t.Errorf("a config with no min_free_bytes loaded with a floor of %d, want the default %d",
			cfg.Limits.MinFreeBytes, Defaults().Limits.MinFreeBytes)
	}
}

// Somebody setting this up knows their broker as an address on their network, not as a URL with
// a scheme. Rejecting what they know would send them looking up what "tcp://" means.
func TestABrokerAddressIsCompletedRatherThanRejected(t *testing.T) {
	for _, typed := range []struct {
		broker string
		want   string
	}{
		{"192.168.1.10", "tcp://192.168.1.10:1883"},
		{"192.168.1.10:1884", "tcp://192.168.1.10:1884"},
		{"tcp://homeassistant.local", "tcp://homeassistant.local:1883"},
		{"ssl://broker.example", "ssl://broker.example:8883"},
		{" 192.168.1.10 ", "tcp://192.168.1.10:1883"},
	} {
		cfg := Defaults()
		cfg.MQTT.Broker = typed.broker
		if got := cfg.MQTT.BrokerURL(); got != typed.want {
			t.Errorf("a broker of %q is dialled as %q, want %q", typed.broker, got, typed.want)
		}
		if err := cfg.Validate(); err != nil {
			t.Errorf("a broker of %q was rejected: %v", typed.broker, err)
		}
	}
}

// The address field is the one people paste a whole URL into, and a whole URL from a broker's
// own documentation has the credentials in it. Wherever that address goes afterwards — the dial
// log line, the value attribute of the settings form, a complaint that it will not parse — the
// password must not go with it.
func TestAPasswordTypedIntoTheAddressIsMovedOutOfIt(t *testing.T) {
	for _, typed := range []struct {
		broker string
		want   string
	}{
		{"mqtt://someone:hunter2@192.168.1.10", "mqtt://192.168.1.10:1883"},
		{"someone:hunter2@192.168.1.10:1884", "tcp://192.168.1.10:1884"},
		{"ssl://someone:hunter2@broker.example/mqtt", "ssl://broker.example:8883/mqtt"},
	} {
		settings := Defaults().MQTT
		settings.Broker = typed.broker

		if got := settings.BrokerURL(); got != typed.want {
			t.Errorf("a broker of %q is dialled as %q, want %q", typed.broker, got, typed.want)
		}

		moved := settings.WithoutBrokerUserinfo()
		switch {
		case strings.Contains(moved.Broker, "hunter2"):
			t.Errorf("the stored address is %q, which still carries the password", moved.Broker)
		case moved.Username != "someone" || moved.Password != "hunter2":
			t.Errorf("a broker of %q logs in as %q/%q, so the credentials were lost",
				typed.broker, moved.Username, moved.Password)
		}
	}
}

// An address too malformed to read is one something is about to complain about, and the complaint
// is shown on the settings page. Its credentials go rather than being kept: the client reads the
// address the same way and will not dial one it cannot read, so there was nothing to keep.
func TestAnAddressTooMalformedToReadStillLosesItsPassword(t *testing.T) {
	settings := Defaults().MQTT
	settings.Broker = "mqtt://someone:hunter2%zz@192.168.1.10"

	if moved := settings.WithoutBrokerUserinfo().Broker; strings.Contains(moved, "hunter2") {
		t.Errorf("the stored address is %q, which still carries the password", moved)
	}

	cfg := Defaults()
	cfg.MQTT.Broker = "mqtt://someone:hunter2%zz@"
	err := cfg.Validate()
	if err == nil {
		t.Fatal("an address with no host at all was accepted")
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Errorf("the rejection reads %q, which quotes the password back", err)
	}
}

// An empty broker is the off switch, so it has to pass validation while everything else about the
// section is left at whatever it was.
func TestNoBrokerMeansTheIntegrationIsOffRatherThanBroken(t *testing.T) {
	cfg := Defaults()
	if cfg.MQTT.Enabled() {
		t.Error("a fresh config is already publishing to a broker")
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("a config with no broker was rejected: %v", err)
	}
}

func TestValidateRejectsTopicPrefixesThatAreNotTopics(t *testing.T) {
	cfg := Defaults()
	cfg.MQTT.Broker = "192.168.1.10"
	cfg.MQTT.TopicPrefix = "gpb/#"
	if err := cfg.Validate(); err == nil {
		t.Error("a topic prefix containing a wildcard was accepted")
	}

	cfg = Defaults()
	cfg.MQTT.Broker = "192.168.1.10"
	cfg.MQTT.TopicPrefix = ""
	if err := cfg.Validate(); err == nil {
		t.Error("an empty topic prefix was accepted alongside a broker")
	}
}

// Every install that upgrades into this feature has a config file with no tls table in it, and
// must go on answering on the address it always did. Turning encryption on for somebody who never
// asked would take their UI away — the browser talks http, the server answers https, and nothing
// on the page says why.
func TestAConfigWrittenBeforeEncryptionExistedStaysOnPlainHTTP(t *testing.T) {
	dir := t.TempDir()
	older := "photos_dir = \"/photos\"\n\n[web]\nlisten = \":8080\"\nexternal_url = \"http://192.168.1.10:8090\"\n"
	if err := os.WriteFile(Path(dir), []byte(older), 0o600); err != nil {
		t.Fatalf("writing an older config: %v", err)
	}

	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("loading an older config: %v", err)
	}
	if cfg.Web.TLS.Enabled() {
		t.Errorf("a config with no tls table loaded as %q, want it off", cfg.Web.TLS.Mode)
	}
	if cfg.Web.TLS.Scheme() != "http" {
		t.Errorf("the scheme is %q, want http", cfg.Web.TLS.Scheme())
	}
}

// The mode decides whether the daemon can listen at all, so a value it does not understand has to
// be refused where it is typed. The empty string is the exception: it plainly means off, and the
// cost of being strict about it is an appliance that will not start and a settings page nobody
// can reach to correct it.
func TestAnEncryptionModeThatIsNotOneOfTheThreeIsRefused(t *testing.T) {
	for _, mode := range []TLSMode{"", TLSOff, TLSSelfSigned} {
		cfg := Defaults()
		cfg.Web.TLS.Mode = mode
		if err := cfg.Validate(); err != nil {
			t.Errorf("web.tls.mode %q was refused: %v", mode, err)
		}
	}

	cfg := Defaults()
	cfg.Web.TLS.Mode = "https"
	if err := cfg.Validate(); err == nil {
		t.Error("web.tls.mode \"https\" was accepted, and the daemon would refuse to listen with it")
	}
}

// A certificate the user provides is two files, and one of them is no use. Catching it here is
// what keeps the failure on the settings page rather than in a daemon that will not start.
func TestAProvidedCertificateNeedsBothHalves(t *testing.T) {
	for _, half := range []TLS{
		{Mode: TLSProvided},
		{Mode: TLSProvided, CertFile: "/data/tls/cert.pem"},
		{Mode: TLSProvided, KeyFile: "/data/tls/key.pem"},
	} {
		cfg := Defaults()
		cfg.Web.TLS = half
		if err := cfg.Validate(); err == nil {
			t.Errorf("cert_file %q with key_file %q was accepted", half.CertFile, half.KeyFile)
		}
	}

	cfg := Defaults()
	cfg.Web.TLS = TLS{Mode: TLSProvided, CertFile: "/data/tls/cert.pem", KeyFile: "/data/tls/key.pem"}
	if err := cfg.Validate(); err != nil {
		t.Errorf("a certificate and key together were refused: %v", err)
	}
}

// The mode survives being written and read back, which is the whole of what this setting has to
// do between one start and the next.
func TestTheEncryptionModeSurvivesTheRoundTrip(t *testing.T) {
	dir := t.TempDir()
	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("loading a fresh config: %v", err)
	}

	cfg.Web.TLS = TLS{Mode: TLSSelfSigned}
	cfg.Web.ExternalURL = "https://192.168.1.10:8090"
	if err := cfg.Save(); err != nil {
		t.Fatalf("saving: %v", err)
	}

	reloaded, err := Load(dir)
	if err != nil {
		t.Fatalf("loading it back: %v", err)
	}
	if reloaded.Web.TLS.Mode != TLSSelfSigned || !reloaded.Web.TLS.Enabled() {
		t.Errorf("the mode came back as %q", reloaded.Web.TLS.Mode)
	}
	if reloaded.Web.TLS.Scheme() != "https" {
		t.Errorf("the scheme came back as %q, want https", reloaded.Web.TLS.Scheme())
	}
}
