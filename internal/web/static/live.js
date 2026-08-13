// Live regions. A region is an element that names the topics it cares about and where to fetch
// itself from; the socket only ever carries a topic name, so what lands on the page is always the
// server's own rendering, asked for at the moment it was wanted rather than pushed and stored.
//
// Because a message is only a hint to look again, missing one costs nothing: a client coming back
// from a broken connection or a hidden tab asks for every region it has instead of replaying what
// it did not hear.
//
// The one navigation here is to the login page when the session has expired, because a page that
// silently stops updating is worse than one that says where to go.
(function () {
  if (!document.querySelector("[data-live]")) return;

  const settleMs = 200;
  const firstRetryMs = 1000;
  const longestRetryMs = 30000;

  let retryMs = firstRetryMs;
  const settling = new Map();

  connect();

  // A tab that was in the background missed nothing it can name, so it re-asks for everything.
  document.addEventListener("visibilitychange", () => {
    if (document.visibilityState === "visible") refreshEveryRegion();
  });

  function connect() {
    const scheme = location.protocol === "https:" ? "wss:" : "ws:";
    const socket = new WebSocket(`${scheme}//${location.host}/ws`);

    socket.addEventListener("open", () => {
      retryMs = firstRetryMs;
      refreshEveryRegion();
    });
    socket.addEventListener("message", (message) => {
      refreshRegionsFor(JSON.parse(message.data).topic);
    });
    socket.addEventListener("close", () => {
      // The daemon restarting is exactly when this fires, and the page still shows the truth as
      // of a moment ago, so it is not worth a banner — only worth trying again, less eagerly.
      setTimeout(connect, retryMs);
      retryMs = Math.min(retryMs * 2, longestRetryMs);

      // A restart also empties the session store, and a socket refused for that reason would
      // reconnect for ever against a server that will never have it. Asking for one region says
      // which it is: an expired session goes to the login page, and a server that is merely down
      // fails the fetch and changes nothing.
      const [first] = regions();
      if (first) pull(first.id);
    });
  }

  function regions() {
    return Array.from(document.querySelectorAll("[data-live]"));
  }

  function refreshRegionsFor(topic) {
    regions()
      .filter((region) => region.dataset.live.split(" ").includes(topic))
      .forEach(settleThenPull);
  }

  function refreshEveryRegion() {
    regions().forEach(settleThenPull);
  }

  // Two topics arriving in one tick would otherwise fetch a region that answers to both twice.
  function settleThenPull(region) {
    clearTimeout(settling.get(region.id));
    settling.set(
      region.id,
      setTimeout(() => {
        settling.delete(region.id);
        pull(region.id);
      }, settleMs),
    );
  }

  function pull(id) {
    const region = document.getElementById(id);
    if (!region) return;

    if (holdsTheReadersHands(region)) {
      region.addEventListener("focusout", () => settleThenPull(region), { once: true });
      return;
    }

    fetch(region.dataset.liveSrc, { headers: { Accept: "text/html" } })
      .then((response) => {
        if (response.status === 401) {
          location.assign(`/login?next=${encodeURIComponent(location.pathname)}`);
          return null;
        }
        if (!response.ok) throw new Error(`HTTP ${response.status}`);
        return response.text();
      })
      .then((html) => {
        if (html === null) return;
        // The markup is this application's own, out of the same html/template set that rendered
        // the region being replaced, and escaped exactly as a full page render would escape it.
        // Replacing the whole element rather than its contents keeps the data attributes, which
        // the block renders, so the region is still a region next time.
        region.outerHTML = html;
        // Markup that arrives this way has no listeners on it. The scripts that make a grid work
        // bind on this and skip whatever they have already bound.
        document.dispatchEvent(new CustomEvent("gpb:swapped"));
      })
      .catch(() => {
        // A fetch that failed leaves the region showing what it showed before. That is stale, but
        // it is the server's own last word rather than a guess, and the next topic will fix it.
      });
  }

  // A half-typed password, or the nav link a keyboard is halfway along, is worth more than a fresh
  // rendering of it, so the swap waits for the hands to leave rather than taking what they were in
  // the middle of. Whatever is put off this way is asked for again on focusout.
  function holdsTheReadersHands(region) {
    const focused = document.activeElement;
    return focused !== null && region.contains(focused);
  }
})();
