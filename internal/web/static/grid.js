// The item grid's picking behaviour. Every cell is a real checkbox inside a real form, which is
// what makes the keyboard and the no-script browser work without any of this. What this adds is
// saving without a page reload — on a grid of two hundred, a reload per photo is unusable — and
// shift-click ranges, which a checkbox cannot do on its own.
//
// A grid can also arrive after the page did, when an album being walked for the first time
// finishes, so this binds on load and again on whatever the socket swapped in.
(function () {
  wireGrid();
  document.addEventListener("gpb:swapped", wireGrid);

  function wireGrid() {
    const grid = document.getElementById("grid");
    const tools = document.getElementById("picktools");
    if (!grid || !tools || grid.dataset.wired) return;
    // Every swapped region fires the same event, so the marker is what keeps a nav or banner
    // update from binding this grid a second time.
    grid.dataset.wired = "true";

    const albumID = tools.dataset.album;
    const counter = document.getElementById("pickcount");
    const cells = Array.from(grid.querySelectorAll(".grid-cell"));

    // The Save button is the no-script path's whole point, and this is the script: from here on
    // every tick saves itself, so a button offering to do it again is a button that does nothing.
    document.getElementById("savepicks")?.remove();

    // anchor is where the last plain click landed, which is what a shift-click measures from.
    let anchor = null;

    grid.addEventListener("click", (event) => {
      const cell = event.target.closest(".grid-cell");
      if (!cell) return;
      // The viewer's button sits on the cell and means "show me this", not "back this up".
      if (event.target.closest("[data-open]")) return;

      // Taking the toggle over from the browser is what lets one gesture pick a whole range rather
      // than the one box under the pointer. Space on a focused checkbox arrives here as a click
      // too, so the keyboard goes down exactly the same path and saves exactly the same way.
      event.preventDefault();

      const selected = !ticked(cell);
      const affected = event.shiftKey && anchor ? range(anchor, cell) : [cell];

      // Painted before the request answers: a grid that waits for the network to show a tick feels
      // broken. A failure repaints from what the server still believes.
      affected.forEach((each) => setTicked(each, selected));
      anchor = cell;

      send({ mediaKeys: affected.map(keyOf), selected: selected }, affected, !selected);
    });

    tools.addEventListener("click", (event) => {
      const button = event.target.closest("button[data-scope]");
      if (!button) return;

      const selected = button.dataset.selected === "true";
      cells.forEach((cell) => setTicked(cell, selected));
      send({ scope: "album", selected: selected }, cells, !selected);
    });

    function boxIn(cell) {
      return cell.querySelector("input[type=checkbox]");
    }

    function ticked(cell) {
      return boxIn(cell).checked;
    }

    function setTicked(cell, selected) {
      boxIn(cell).checked = selected;
    }

    function keyOf(cell) {
      return boxIn(cell).value;
    }

    function range(from, to) {
      const first = cells.indexOf(from);
      const last = cells.indexOf(to);
      if (first < 0 || last < 0) return [to];
      return cells.slice(Math.min(first, last), Math.max(first, last) + 1);
    }

    function send(body, affected, revertTo) {
      fetch(`/album/${encodeURIComponent(albumID)}/picks`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(body),
      })
        .then((response) => {
          if (response.status === 401) return window.gpb.signInAgain();
          if (!response.ok) throw new Error(`HTTP ${response.status}`);
          return response.json();
        })
        .then((totals) => {
          if (totals && counter) counter.textContent = totals.picked.toLocaleString("en-US");
        })
        .catch((error) => {
          // Put the cells back the way the server still believes they are, and say so — a silently
          // lost pick would be discovered as a missing photo months later.
          affected.forEach((cell) => setTicked(cell, revertTo));
          report(`That change was not saved (${error.message}).`);
        });
    }

    const report = window.gpb.bannerIn("pickerror", () => tools);
  }
})();
