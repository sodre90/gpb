package web

import (
	"context"
	"encoding/json"
	"log"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/gobwas/ws"
	"github.com/gobwas/ws/wsutil"

	"gpb/internal/auth"
	"gpb/internal/syncer"
)

// The topics a region can subscribe to. They are coarse on purpose: a topic says only that
// something in that part of the world moved, and the client answers it by asking the server to
// render the region again. Nothing on the wire is ever load-bearing, which is what makes a
// dropped message, a stalled reader and a reconnect all cost the same as nothing.
const (
	topicRun      = "run"
	topicProgress = "progress"
	topicSession  = "session"
	topicReview   = "review"
)

// layoutHost names a page whose template set is used to render the nav and the session banner.
// They are defined in the layout, which every page carries, so any of them would do.
const layoutHost = "overview"

// watchInterval bounds how late a region can be. It replaces what used to be one HTTP request
// per open page every five seconds with one in-process comparison, whoever is watching.
const watchInterval = time.Second

// liveWriteTimeout reaps a socket whose other end has gone away without saying so — a closed
// laptop lid leaves a connection that accepts writes into a buffer nobody will ever drain.
const liveWriteTimeout = 5 * time.Second

type hub struct {
	mu      sync.Mutex
	clients map[*liveClient]struct{}
}

func newHub() *hub { return &hub{clients: map[*liveClient]struct{}{}} }

func (h *hub) register(client *liveClient) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.clients[client] = struct{}{}
}

func (h *hub) unregister(client *liveClient) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.clients, client)
}

// publish reaches every open socket rather than only the ones on a page that could use the
// topic. A client that has no region for it drops it on the floor, which is cheaper than the
// bookkeeping the server would need to know what each connection is looking at.
func (h *hub) publish(topic string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for client := range h.clients {
		client.queue(topic)
	}
}

// closeAll exists because http.Server.Shutdown deliberately leaves hijacked connections alone,
// and every socket here is one.
func (h *hub) closeAll() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for client := range h.clients {
		client.conn.Close()
	}
}

func (h *hub) connected() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.clients)
}

// liveClient is one open socket. Topics waiting to go out are held as a set rather than a queue:
// a reader that stalls for a minute wakes to the same handful of names it would have received one
// at a time, so it can neither miss an event nor grow a backlog, and publish never blocks.
type liveClient struct {
	conn    net.Conn
	mu      sync.Mutex
	pending map[string]struct{}
	wake    chan struct{}
	done    chan struct{}
}

func newLiveClient(conn net.Conn) *liveClient {
	return &liveClient{
		conn:    conn,
		pending: map[string]struct{}{},
		wake:    make(chan struct{}, 1),
		done:    make(chan struct{}),
	}
}

func (c *liveClient) queue(topic string) {
	c.mu.Lock()
	c.pending[topic] = struct{}{}
	c.mu.Unlock()

	select {
	case c.wake <- struct{}{}:
	default:
	}
}

func (c *liveClient) take() []string {
	c.mu.Lock()
	defer c.mu.Unlock()

	topics := make([]string, 0, len(c.pending))
	for topic := range c.pending {
		topics = append(topics, topic)
	}
	clear(c.pending)
	return topics
}

func (c *liveClient) writeLoop() {
	for {
		select {
		case <-c.done:
			return
		case <-c.wake:
			for _, topic := range c.take() {
				if err := c.send(topic); err != nil {
					c.conn.Close()
					return
				}
			}
		}
	}
}

func (c *liveClient) send(topic string) error {
	message, err := json.Marshal(struct {
		Topic string `json:"topic"`
	}{Topic: topic})
	if err != nil {
		return err
	}

	c.conn.SetWriteDeadline(time.Now().Add(liveWriteTimeout))
	return wsutil.WriteServerText(c.conn, message)
}

// readLoop expects nothing. The client never sends data; this reads so that pings are answered
// and a close frame is noticed rather than waited out.
func (c *liveClient) readLoop() {
	defer close(c.done)
	for {
		if _, _, err := wsutil.ReadClientData(c.conn); err != nil {
			c.conn.Close()
			return
		}
	}
}

func (s *Server) handleLiveSocket(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		http.Error(w, "cross-origin socket refused", http.StatusForbidden)
		return
	}

	conn, _, _, err := ws.UpgradeHTTP(r, w)
	if err != nil {
		return
	}

	client := newLiveClient(conn)
	s.live.register(client)

	go client.writeLoop()
	go func() {
		defer s.live.unregister(client)
		client.readLoop()
	}()
}

// sameOrigin is this handler's own business because the cross-origin protection wrapping the mux
// guards unsafe methods only, and a socket upgrade is a GET. A browser always sends Origin on an
// upgrade, so a request without one is not a page and has nothing to subscribe to.
func sameOrigin(r *http.Request) bool {
	origin, err := url.Parse(r.Header.Get("Origin"))
	if err != nil || origin.Host == "" {
		return false
	}
	return origin.Host == r.Host
}

type liveSnapshot struct {
	activity      string
	progress      syncer.Progress
	authState     auth.State
	warming       bool
	reauthRunning bool
	waitingForYou int
}

func (s *Server) liveSnapshot() liveSnapshot {
	status := s.auth.Status()
	return liveSnapshot{
		activity:      s.runs.Activity(),
		progress:      s.runs.Progress(),
		authState:     status.State,
		warming:       status.Warming,
		reauthRunning: s.reauth.Running(),
		waitingForYou: s.waitingForReview(),
	}
}

func changedTopics(before, after liveSnapshot) []string {
	var topics []string
	if before.activity != after.activity {
		topics = append(topics, topicRun)
	}
	if !before.progress.Equal(after.progress) {
		topics = append(topics, topicProgress)
	}
	if before.authState != after.authState ||
		before.warming != after.warming ||
		before.reauthRunning != after.reauthRunning {
		topics = append(topics, topicSession)
	}
	if before.waitingForYou != after.waitingForYou {
		topics = append(topics, topicReview)
	}
	return topics
}

// watchForChanges is why nothing outside this package had to learn that a socket exists. Rather
// than threading a publish hook down through the runner and the syncer, the server compares what
// it already renders from against what it saw a moment ago.
func (s *Server) watchForChanges(ctx context.Context) {
	ticker := time.NewTicker(watchInterval)
	defer ticker.Stop()

	previous := s.liveSnapshot()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			current := s.liveSnapshot()
			for _, topic := range changedTopics(previous, current) {
				s.live.publish(topic)
			}
			previous = current
		}
	}
}

func (s *Server) handleLiveOverview(w http.ResponseWriter, r *http.Request) {
	view, err := s.overviewView()
	if err != nil {
		log.Printf("web: building the overview: %v", err)
		http.Error(w, "the overview is unavailable", http.StatusInternalServerError)
		return
	}
	writeFragment(w, "overview", "overviewlive", view)
}

// handleLiveRunsNow answers with the Now card's container, which is why it has no empty case: a
// run that has ended renders the container with nothing in it, and the next run to start renders
// the card back into a page that never reloaded to find out.
func (s *Server) handleLiveRunsNow(w http.ResponseWriter, r *http.Request) {
	writeFragment(w, "runs", "livenow", s.nowCard())
}

func (s *Server) handleLiveRunsHistory(w http.ResponseWriter, r *http.Request) {
	view, err := s.runsView()
	if err != nil {
		log.Printf("web: building the run history: %v", err)
		http.Error(w, "the run history is unavailable", http.StatusInternalServerError)
		return
	}
	writeFragment(w, "runs", "runhistory", view)
}

func (s *Server) handleLiveAlbumsNow(w http.ResponseWriter, r *http.Request) {
	view, err := s.albumsView(queryFrom(r))
	if err != nil {
		log.Printf("web: building the album list: %v", err)
		http.Error(w, "the album list is unavailable", http.StatusInternalServerError)
		return
	}
	writeFragment(w, "albums", "albumsnow", view)
}

// handleLiveAlbumListing reports on a first-time walk without starting one. The page handler asks
// for a listing when it finds an album nobody has opened before; a fragment that did the same
// would start a fresh walk every time the socket said the last one had moved.
func (s *Server) handleLiveAlbumListing(w http.ResponseWriter, r *http.Request) {
	view, err := s.albumView(r.PathValue("id"), 1)
	if err != nil {
		log.Printf("web: building the item grid: %v", err)
		http.Error(w, "that album is unavailable", http.StatusNotFound)
		return
	}

	view.Activity = s.runs.Activity()
	view.Running = view.Activity != ""
	writeFragment(w, "album", "albumlisting", view)
}

// handleLiveNav re-renders the whole top bar rather than the review pill alone, because the pill
// is a child of the link that describes it to a screen reader and the two have to change together.
func (s *Server) handleLiveNav(w http.ResponseWriter, r *http.Request) {
	on := r.URL.Query().Get("on")
	writeFragment(w, layoutHost, "navbar", pageData{
		Authenticated: true,
		Nav:           s.nav(on),
		NavSrc:        navSrc(on),
	})
}

func navSrc(path string) string {
	return "/live/nav?on=" + url.QueryEscape(path)
}

// handleLiveSessionBanner answers with the banner's container even when there is nothing to say,
// so the place a warning would appear stays on the page waiting for one.
func (s *Server) handleLiveSessionBanner(w http.ResponseWriter, r *http.Request) {
	writeFragment(w, layoutHost, "sessionbanner", pageData{
		Authenticated: true,
		Session:       s.sessionBanner(),
	})
}

func (s *Server) handleLiveReauthStatus(w http.ResponseWriter, r *http.Request) {
	writeFragment(w, "reauth", "authstatus", s.statusView())
}

func (s *Server) handleLiveReauthActions(w http.ResponseWriter, r *http.Request) {
	writeFragment(w, "reauth", "authactions", s.reauthView(r))
}

func writeFragment(w http.ResponseWriter, page, block string, data any) {
	rendered, err := renderBlock(page, block, data)
	if err != nil {
		log.Printf("web: rendering the %s fragment: %v", block, err)
		http.Error(w, "that part of the page could not be rendered", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(rendered))
}
