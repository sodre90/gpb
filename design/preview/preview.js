// Prototype-only script. It demonstrates the two JS enhancements the design proposes —
// the album filter box and grid picking — without any server behind them. In the real app
// these live in separate files and POST their changes (see WEB-DESIGN.md §5).
(function () {
  const filter = document.querySelector("input[data-filter]");
  if (filter) {
    const rows = Array.from(document.querySelectorAll("table.albums tbody tr"));
    const tally = document.getElementById("tally");
    const total = tally ? tally.textContent : "";

    filter.addEventListener("input", () => {
      const needle = filter.value.trim().toLowerCase();
      let shown = 0;
      rows.forEach((row) => {
        const match = !needle || row.querySelector("td:nth-child(2)").textContent.toLowerCase().includes(needle);
        row.hidden = !match;
        if (match) shown += 1;
      });
      if (tally) tally.textContent = needle ? `${shown} of ${rows.length} albums match.` : total;
    });
  }

  const grid = document.querySelector(".grid.picking");
  if (grid) {
    const boxes = Array.from(grid.querySelectorAll("input[type=checkbox]"));
    const counter = document.getElementById("pickcount");
    let anchor = null;

    // The real script also removes the no-JS "Save picks" button and POSTs each change;
    // here the checkboxes just flip, which is enough to feel the interaction.
    document.querySelectorAll("[data-nojs-only]").forEach((button) => button.remove());

    grid.addEventListener("click", (event) => {
      const box = event.target.closest(".cell")?.querySelector("input[type=checkbox]");
      if (!box) return;

      if (event.shiftKey && anchor) {
        event.preventDefault();
        const [from, to] = [boxes.indexOf(anchor), boxes.indexOf(box)].sort((a, b) => a - b);
        boxes.slice(from, to + 1).forEach((each) => (each.checked = anchor.checked));
      }
      anchor = box;
      recount();
    });

    grid.addEventListener("change", recount);

    function recount() {
      if (counter) counter.textContent = boxes.filter((box) => box.checked).length;
    }
  }
})();
