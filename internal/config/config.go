package config

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

const (
	defaultDataDir   = "/data"
	defaultPhotosDir = "/photos"
	configFileName   = "config.toml"
)

// defaultMinFreeBytes is the free space a run refuses to eat into. It is deliberately generous:
// the pool shares its filesystem with whatever else the machine keeps there, and a backup is the
// one process on it that will consume every byte it is given. Zero, written by hand, turns the
// check off. The syncer enforces it and holds the same number as its own fallback; the two are
// kept honest by a test in internal/engine, where both packages are already in scope.
const defaultMinFreeBytes = 8 << 30

type Config struct {
	PhotosDir string   `toml:"photos_dir"`
	Web       Web      `toml:"web"`
	Schedule  Schedule `toml:"schedule"`
	Limits    Limits   `toml:"limits"`
	Thumbs    Thumbs   `toml:"thumbs"`
	Notify    Notify   `toml:"notify"`
	MQTT      MQTT     `toml:"mqtt"`

	dataDir string
}

type Web struct {
	Listen         string   `toml:"listen"`
	ExternalURL    string   `toml:"external_url"`
	PasswordHash   string   `toml:"password_hash"`
	SessionIdleTTL Duration `toml:"session_idle_ttl"`
	TLS            TLS      `toml:"tls"`
}

// TLS is whether this UI is served over https and what with. It matters more here than the size
// of the thing suggests: the password typed into this page unlocks a Chrome profile that is a
// whole Google account, not a Photos-scoped token, and the session cookie behind it is a bearer
// token for the same. Over plain http both cross the LAN in the clear.
type TLS struct {
	Mode     TLSMode `toml:"mode"`
	CertFile string  `toml:"cert_file"`
	KeyFile  string  `toml:"key_file"`
}

type TLSMode string

const (
	TLSOff TLSMode = "off"
	// TLSSelfSigned is the case a home LAN is actually in: no CA, no domain name, and a
	// certificate this program makes for itself. It encrypts the wire and proves nothing about
	// who is on the other end of it, and the settings page says so rather than letting a browser
	// warning read as a fault.
	TLSSelfSigned TLSMode = "self-signed"
	TLSProvided   TLSMode = "provided"
)

func (t TLS) Enabled() bool {
	return t.Mode == TLSSelfSigned || t.Mode == TLSProvided
}

func (t TLS) Scheme() string {
	if t.Enabled() {
		return "https"
	}
	return "http"
}

// An empty mode is off rather than a fault: Validate runs on every load, and a config written
// before this setting existed keeps the default from Defaults — but one hand-edited to an empty
// string should still start the daemon rather than lock its owner out of the page that would fix
// it. A mode that is neither empty nor one of the three is a typo worth refusing.
func (t TLS) Validate() error {
	switch t.Mode {
	case "", TLSOff, TLSSelfSigned:
		return nil
	case TLSProvided:
		if strings.TrimSpace(t.CertFile) == "" || strings.TrimSpace(t.KeyFile) == "" {
			return fmt.Errorf("web.tls.mode is %q, so web.tls.cert_file and web.tls.key_file must both be set", t.Mode)
		}
		return nil
	}
	return fmt.Errorf("web.tls.mode %q is not one of off, self-signed or provided", t.Mode)
}

type Schedule struct {
	SyncAt            string   `toml:"sync_at"`
	KeepaliveInterval Duration `toml:"keepalive_interval"`
}

type Limits struct {
	DownloadWorkers   int     `toml:"download_workers"`
	RequestsPerSecond float64 `toml:"requests_per_second"`
	// MinFreeBytes is the headroom a run refuses to eat into, and zero turns the check off
	// altogether. That is why the key must never be absent from a file this program writes:
	// Load decodes over Defaults, so a missing key keeps the floor, but a file saved without
	// the key would come back as no floor at all.
	MinFreeBytes int64 `toml:"min_free_bytes"`
}

type Thumbs struct {
	CacheMaxBytes     int64   `toml:"cache_max_bytes"`
	RequestsPerSecond float64 `toml:"requests_per_second"`
}

type Notify struct {
	Command string `toml:"command"`
}

// MQTT is how this backup shows up in Home Assistant. An empty broker turns the whole thing off,
// which is why there is no separate enabled flag: a broker nobody set is a broker nobody wanted.
type MQTT struct {
	Broker   string `toml:"broker"`
	Username string `toml:"username"`
	Password string `toml:"password"`
	// TopicPrefix is where this program's own topics live. DiscoveryPrefix is where Home
	// Assistant looks for things announcing themselves, and is its setting rather than ours —
	// changing it here without changing it there means HA never sees any of this.
	TopicPrefix     string `toml:"topic_prefix"`
	DiscoveryPrefix string `toml:"discovery_prefix"`
}

func (m MQTT) Enabled() bool {
	return strings.TrimSpace(m.Broker) != ""
}

// BrokerURL is what the client is handed. Somebody setting this up thinks of their broker as an
// address, not a URL, so a bare host or host:port is completed rather than rejected — which is
// the difference between typing what you know and looking up what a scheme is called.
//
// It never carries a username or password, whatever was typed into the address. This string is
// logged on every dial and quoted back when the address will not parse, and the credentials are
// kept in their own fields by WithoutBrokerUserinfo.
func (m MQTT) BrokerURL() string {
	broker := withoutUserinfo(m.completedBroker())
	if broker == "" {
		return ""
	}

	parsed, err := url.Parse(broker)
	if err != nil {
		return broker
	}
	if parsed.Port() == "" {
		if port := defaultPortFor(parsed.Scheme); port != "" {
			parsed.Host = net.JoinHostPort(parsed.Host, port)
		}
	}
	return parsed.String()
}

func (m MQTT) completedBroker() string {
	broker := strings.TrimSpace(m.Broker)
	if broker == "" || strings.Contains(broker, "://") {
		return broker
	}
	return "tcp://" + broker
}

// WithoutBrokerUserinfo moves a username and password typed into the address into the fields
// meant for them. A broker's own documentation gives you a whole URL and that is what gets
// pasted — but the address is logged when it is dialled and rendered back into the settings form,
// while the password field beside it is never rendered back at all. The credentials go on
// working; they are only kept where the rest of the program already looks for them.
// An address too malformed to read loses its credentials rather than keeping them, since they
// were never going to work: the client parses the address the same way and declines to dial one
// it cannot read at all.
func (m MQTT) WithoutBrokerUserinfo() MQTT {
	broker := m.completedBroker()

	parsed, err := url.Parse(broker)
	if err != nil {
		if stripped := withoutUserinfo(broker); stripped != broker {
			m.Broker = stripped
		}
		return m
	}
	if parsed.User == nil {
		return m
	}

	m.Username = parsed.User.Username()
	if password, set := parsed.User.Password(); set {
		m.Password = password
	}
	parsed.User = nil
	m.Broker = parsed.String()
	return m
}

// withoutUserinfo cuts the credentials out of an address as text rather than by parsing it,
// because an address too malformed to parse is an address that gets quoted in a complaint about
// how malformed it is — and a password typed into it would be quoted with it.
func withoutUserinfo(broker string) string {
	scheme, rest, isURL := strings.Cut(broker, "://")
	if !isURL {
		return broker
	}

	authority, path, hasPath := strings.Cut(rest, "/")
	if _, afterCredentials, hasUserinfo := strings.Cut(authority, "@"); hasUserinfo {
		authority = afterCredentials
	}
	if hasPath {
		return scheme + "://" + authority + "/" + path
	}
	return scheme + "://" + authority
}

func defaultPortFor(scheme string) string {
	switch scheme {
	case "tcp", "mqtt":
		return "1883"
	case "ssl", "tls", "mqtts":
		return "8883"
	default:
		return ""
	}
}

func (m MQTT) validate() error {
	if !m.Enabled() {
		return nil
	}

	// Hostname rather than Host: completing a scheme with no address at all leaves ":1883",
	// which is a host as far as url is concerned and nowhere as far as a network is.
	parsed, err := url.Parse(m.BrokerURL())
	if err != nil || parsed.Hostname() == "" {
		return fmt.Errorf("mqtt.broker %q is not an address such as 192.168.1.10 or tcp://192.168.1.10:1883", m.BrokerURL())
	}
	if m.TopicPrefix == "" || m.DiscoveryPrefix == "" {
		return fmt.Errorf("mqtt.topic_prefix and mqtt.discovery_prefix cannot be empty while a broker is set")
	}
	// A wildcard in a topic we publish to is not a topic at all, and a subscription built from
	// one would listen to a great deal more than this program's own commands.
	if strings.ContainsAny(m.TopicPrefix+m.DiscoveryPrefix, "+#") {
		return fmt.Errorf("mqtt topic prefixes cannot contain + or #")
	}
	return nil
}

type Duration struct {
	time.Duration
}

func (d *Duration) UnmarshalText(text []byte) error {
	parsed, err := time.ParseDuration(string(text))
	if err != nil {
		return err
	}
	d.Duration = parsed
	return nil
}

func (d Duration) MarshalText() ([]byte, error) {
	return []byte(d.String()), nil
}

func Defaults() Config {
	return Config{
		PhotosDir: defaultPhotosDir,
		Web: Web{
			Listen: ":8080",
			// No default: the address this UI answers on from outside the container is not
			// something the process can know, and a wrong one in a notification is worse than
			// none — it sends the user to a page that does not answer.
			ExternalURL:    "",
			SessionIdleTTL: Duration{30 * 24 * time.Hour},
			// Off, and written out as such: an install that upgrades into this feature must
			// answer on the address it always did until its owner says otherwise.
			TLS: TLS{Mode: TLSOff},
		},
		Schedule: Schedule{
			SyncAt:            "03:30",
			KeepaliveInterval: Duration{12 * time.Hour},
		},
		Limits: Limits{
			DownloadWorkers:   3,
			RequestsPerSecond: 2.0,
			MinFreeBytes:      defaultMinFreeBytes,
		},
		Thumbs: Thumbs{
			CacheMaxBytes:     1 << 30,
			RequestsPerSecond: 8.0,
		},
		MQTT: MQTT{
			// No default broker: a guess would either fail to connect or, worse, connect to
			// somebody else's. The prefixes are the conventional ones, and homeassistant is
			// what HA itself watches unless it has been told otherwise.
			TopicPrefix:     "gpb",
			DiscoveryPrefix: "homeassistant",
		},
	}
}

func DataDir() string {
	if dir := os.Getenv("GPB_DATA_DIR"); dir != "" {
		return dir
	}
	return defaultDataDir
}

func Path(dataDir string) string {
	return filepath.Join(dataDir, configFileName)
}

func Load(dataDir string) (Config, error) {
	cfg := Defaults()
	cfg.dataDir = dataDir

	path := Path(dataDir)
	if _, err := os.Stat(path); os.IsNotExist(err) {
		if err := cfg.Save(); err != nil {
			return cfg, fmt.Errorf("writing initial config: %w", err)
		}
		return cfg.withEnvOverrides(), nil
	}

	if _, err := toml.DecodeFile(path, &cfg); err != nil {
		return cfg, fmt.Errorf("reading %s: %w", path, err)
	}
	cfg.dataDir = dataDir
	cfg.MQTT = cfg.MQTT.WithoutBrokerUserinfo()
	return cfg.withEnvOverrides(), cfg.Validate()
}

func (c Config) withEnvOverrides() Config {
	if dir := os.Getenv("GPB_PHOTOS_DIR"); dir != "" {
		c.PhotosDir = dir
	}
	return c
}

// Validate is what stops a config being written that the daemon would then refuse to load.
// The web UI edits this file, and a saved setting that bricks the next start is worse than a
// rejected form.
func (c Config) Validate() error {
	if _, err := c.SyncTime(); err != nil {
		return err
	}
	if c.Limits.DownloadWorkers < 1 {
		return fmt.Errorf("limits.download_workers must be at least 1")
	}
	if c.Limits.RequestsPerSecond <= 0 || c.Thumbs.RequestsPerSecond <= 0 {
		return fmt.Errorf("requests_per_second must be positive")
	}
	if c.Limits.MinFreeBytes < 0 {
		return fmt.Errorf("limits.min_free_bytes cannot be negative; use 0 to keep no reserve at all")
	}
	if err := c.MQTT.validate(); err != nil {
		return err
	}
	if err := c.Web.TLS.Validate(); err != nil {
		return err
	}
	if c.Schedule.KeepaliveInterval.Duration < time.Minute {
		return fmt.Errorf("schedule.keepalive_interval must be at least 1m")
	}
	return nil
}

func (c Config) SyncTime() (time.Time, error) {
	parsed, err := time.Parse("15:04", c.Schedule.SyncAt)
	if err != nil {
		return time.Time{}, fmt.Errorf("schedule.sync_at %q is not HH:MM: %w", c.Schedule.SyncAt, err)
	}
	return parsed, nil
}

func (c Config) DataDir() string {
	if c.dataDir == "" {
		return defaultDataDir
	}
	return c.dataDir
}

func (c Config) ProfileDir() string {
	return filepath.Join(c.DataDir(), "profile")
}

// PoolDir is where the photos themselves land. Two parts of the program need to agree on it —
// the run that writes files there and the viewer that reads them back — so it is stated once.
func (c Config) PoolDir() string {
	return filepath.Join(c.PhotosDir, "pool")
}

// TLSDir holds the certificate this program makes for itself. Under the data dir because that
// directory is already 0700 for the Chrome profile's sake, and a private key wants exactly the
// same treatment.
func (c Config) TLSDir() string {
	return filepath.Join(c.DataDir(), "tls")
}

// ThumbCacheDir holds the grid's images. It sits under cache/ because that is exactly what
// it is: deleting it costs re-fetches and nothing else.
func (c Config) ThumbCacheDir() string {
	return filepath.Join(c.DataDir(), "cache", "thumbs")
}

func (c Config) HasPassword() bool {
	return c.Web.PasswordHash != ""
}

func (c Config) Save() error {
	dir := c.DataDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}

	path := Path(dir)
	temporary := path + ".tmp"
	file, err := os.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}

	if err := toml.NewEncoder(file).Encode(c); err != nil {
		file.Close()
		os.Remove(temporary)
		return err
	}
	if err := file.Close(); err != nil {
		os.Remove(temporary)
		return err
	}
	return os.Rename(temporary, path)
}

func (c *Config) SetPassword(password string) error {
	hash, err := HashPassword(password)
	if err != nil {
		return err
	}
	c.Web.PasswordHash = hash
	return c.Save()
}
