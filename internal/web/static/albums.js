// The backup selectors save as they are changed. The Save buttons in the markup are what a
// browser running no script has instead, and this removes the ones it does not need — the album
// rows', where the selector is the only thing on the form. The library card keeps its button,
// because the date beside it should not commit itself half-typed, and that date is the
// difference between a few hundred photos and a hundred thousand.
//
// An album row saves without leaving the page. On a list of two hundred a reload per change
// would throw the reader back to the top every time they changed their mind about one album.
(function () {
  document.querySelectorAll("table.albums tbody tr").forEach(wireRow);
  filterRows();

  const library = document.querySelector("form.library-form");
  if (library) {
    library.querySelector("select").addEventListener("change", () => library.submit());
  }

  // Finding one album among two hundred is the thing sorting cannot do. This hides rows rather
  // than asking the server for a subset: the whole list is already here, and a round-trip per
  // keystroke would be slower than the scan it replaces. The box is hidden in the markup so a
  // browser with no script is never shown a control that would do nothing.
  function filterRows() {
    const box = document.getElementById("filterbox");
    if (!box) return;
    box.hidden = false;

    box.querySelector("input").addEventListener("input", (event) => {
      const wanted = event.target.value.trim().toLowerCase();
      let showing = 0;
      // Asked for afresh each time, not held from load: saving a row replaces the element, and a
      // remembered list would go on filtering the one that is no longer on the page.
      document.querySelectorAll("table.albums tbody tr").forEach((row) => {
        const matches = row.dataset.title.toLowerCase().includes(wanted);
        row.hidden = !matches;
        if (matches) showing++;
      });
      countShown(showing, wanted);
    });
  }

  // The tally counts what the table is showing, or it starts lying the moment a filter hides
  // half of it. The unfiltered sentence is the server's, kept so that clearing the box restores
  // what a reload would say rather than a number this script made up.
  function countShown(showing, wanted) {
    const tally = document.getElementById("tally");
    if (!tally) return;

    if (tally.dataset.all === undefined) tally.dataset.all = tally.textContent;
    tally.textContent = wanted === "" ? tally.dataset.all : `${showing} of ${tally.dataset.all}`;
  }

  function wireRow(row) {
    saveInPlace(row.querySelector("form.mode-form"));
    starInPlace(row.querySelector(".albums-star form"));
  }

  // The star saves in place for the same reason the selector does, and matters more: starring
  // the five albums you care about out of 181 is five reloads of a very long page otherwise.
  function starInPlace(form) {
    form.addEventListener("submit", (event) => {
      event.preventDefault();
      post(form)
        .then((update) => {
          if (!update) return;
          // Unstarring while the list is filtered to favourites takes the row off the page: it
          // is no longer one of the rows this page is showing.
          if (update.gone) {
            form.closest("tr").remove();
          } else {
            replaceRow(form.closest("tr"), update.row, ".albums-star button");
          }
          replaceTally(update.tally);
        })
        .catch((error) => report(`That change was not saved (${error.message}).`));
    });
  }

  function post(form) {
    return fetch(form.action, {
      method: "POST",
      headers: { Accept: "application/json" },
      body: new URLSearchParams(new FormData(form)),
    }).then((response) => {
      if (response.status === 401) return window.gpb.signInAgain();
      if (!response.ok) throw new Error(`HTTP ${response.status}`);
      return response.json();
    });
  }

  function saveInPlace(form) {
    form.querySelectorAll("button[type=submit]").forEach((button) => button.remove());

    const select = form.querySelector("select");
    let saved = select.value;

    select.addEventListener("change", () => {
      const chosen = select.value;
      const focused = document.activeElement === select;

      post(form)
        .then((update) => {
          if (!update) return;
          saved = chosen;
          replaceRow(form.closest("tr"), update.row, focused ? "select" : "");
          replaceTally(update.tally);
        })
        .catch((error) => {
          // Put the selector back to what the server still believes it is, and say so: a change
          // the user watched themselves make and that was not saved would be discovered later
          // as a missing backup.
          select.value = saved;
          report(`That change was not saved (${error.message}).`);
        });
    });
  }

  // The server sends the whole row rather than the parts that moved, so what lands here is what
  // a reload would have shown. Being a new element it needs the behaviour attaching again.
  //
  // The markup is this application's own, out of the same html/template block that rendered the
  // row being replaced — album titles come from Google, and the template escapes them there
  // exactly as it did on the way in.
  function replaceRow(row, html, refocus) {
    const parsed = document.createElement("tbody");
    parsed.innerHTML = html;

    const fresh = parsed.firstElementChild;
    row.replaceWith(fresh);
    wireRow(fresh);

    fresh.classList.add("albums-saved");
    if (refocus) fresh.querySelector(refocus).focus();
  }

  // An account with shared photos but no albums of its own has rows to save and no tally under
  // them, because that line counts albums.
  function replaceTally(html) {
    const tally = document.getElementById("tally");
    if (tally) tally.outerHTML = html;
  }

  const report = window.gpb.bannerIn("saveerror", () => document.querySelector("main h1"));
})();
