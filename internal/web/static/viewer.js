// The viewer. A grid of 256-pixel thumbnails is for finding a photo; this is for looking at one.
// What it shows is the file this backup holds — /image/<key> reads from disk, so a photo already
// backed up is served at full size, costs Google nothing, and still appears when the session is
// dead. Items not yet fetched have no file, so those fall back to the thumbnail and say so.
//
// Grids can arrive after the page did, when an album being walked for the first time finishes,
// so this binds on load and again on whatever the socket swapped in.
(function () {
  let viewer = null;
  // A group at a time: the review page stacks one grid per album, and arrowing out of the album
  // you are deciding about into the next one would lose your place in both.
  let group = [];
  let showing = -1;

  // What the stage is holding and how closely it is being looked at. Scale 1 is the whole
  // photograph in the window; above it the photograph is larger than the window and the pan says
  // which part of it is on screen — which is why moving to another photograph puts back both.
  let framed = null;
  let scale = 1;
  let panX = 0;
  let panY = 0;

  const furthestIn = 8;
  const dragSlop = 3;

  wireGrids();
  document.addEventListener("gpb:swapped", wireGrids);

  function wireGrids() {
    const grids = Array.from(document.querySelectorAll(".grid")).filter(
      (grid) => !grid.dataset.viewerWired && grid.querySelector(".grid-cell[data-key]"));
    if (grids.length === 0) return;

    viewer = viewer || build();
    grids.forEach(wire);
  }

  function wire(grid) {
    grid.dataset.viewerWired = "true";
    const cells = Array.from(grid.querySelectorAll(".grid-cell[data-key]"));

    // The button is hidden in the markup so a scriptless browser is never shown a control that
    // does nothing, and revealed everywhere here because it is also the only way to open a photo
    // from the keyboard.
    cells.forEach((cell) => (cell.querySelector("[data-open]").hidden = false));

    // A cell in a picking grid belongs to the checkbox: a click there means "back this one up",
    // and stealing it for the viewer would make picking impossible. Elsewhere a click on the
    // photo means the obvious thing.
    const picking = grid.classList.contains("grid-picking");

    grid.addEventListener("click", (event) => {
      const cell = event.target.closest(".grid-cell[data-key]");
      if (!cell) return;
      if (picking && !event.target.closest("[data-open]")) return;

      event.preventDefault();
      group = cells;
      show(cells.indexOf(cell));
    });
  }

  function step(by) {
    show((showing + by + group.length) % group.length);
  }

  function show(index) {
    showing = index;
    const cell = group[index];
    const held = cell.hasAttribute("data-held");
    const key = encodeURIComponent(cell.dataset.key);

    framed = mediaFor(cell, held, key);
    stage().replaceChildren(framed);
    fit();

    viewer.querySelector("figcaption").textContent =
      cell.dataset.caption + (held ? "" : " — not backed up yet, so this is Google’s thumbnail");
    viewer.querySelector(".viewer-counter").textContent = `${index + 1} of ${group.length}`;

    if (!viewer.open) viewer.showModal();
  }

  function mediaFor(cell, held, key) {
    if (cell.hasAttribute("data-video") && held) {
      const video = document.createElement("video");
      video.src = `/image/${key}`;
      video.controls = true;
      video.autoplay = true;
      return video;
    }

    const image = document.createElement("img");
    image.src = held ? `/image/${key}` : `/thumb/${key}`;
    image.alt = cell.dataset.caption;
    // Dragging a photo is how you pan it once it is bigger than the window, and the browser's own
    // idea of dragging a picture — carrying it off to another window — would take the gesture.
    image.draggable = false;
    // A file the store believes in but the disk has lost answers 404, and a broken-image icon
    // would say nothing useful. The thumbnail is still there, so show that rather than nothing.
    image.addEventListener("error", () => {
      if (image.src.includes("/image/")) image.src = `/thumb/${key}`;
    });
    return image;
  }

  function stage() {
    return viewer.querySelector(".viewer-stage");
  }

  // Only photographs zoom. A video is already covered in controls that want the same clicks and
  // the same drags, and scrubbing matters more than magnifying.
  function zoomable() {
    return framed instanceof HTMLImageElement;
  }

  function fit() {
    scale = 1;
    panX = 0;
    panY = 0;
    hold();
  }

  // Whatever the scale and pan now say, on the photograph, having first refused to pan further
  // than there is photograph to see.
  function hold() {
    stage().classList.toggle("viewer-near", scale > 1);
    if (!framed) return;

    const spare = (extent, frame) => Math.max(0, (extent * scale - frame) / 2);
    panX = within(panX, spare(framed.offsetWidth, stage().clientWidth));
    panY = within(panY, spare(framed.offsetHeight, stage().clientHeight));
    framed.style.transform = `translate(${panX}px, ${panY}px) scale(${scale})`;
  }

  const within = (value, limit) => Math.min(limit, Math.max(-limit, value));

  // Zoom about a point: whatever was under that point stays under it, so the photograph grows out
  // of where you are looking rather than out of its own middle.
  function zoomAbout(by, clientX, clientY) {
    if (!zoomable()) return;

    const closer = Math.min(furthestIn, Math.max(1, scale * by));
    const growth = closer / scale;
    const shown = framed.getBoundingClientRect();
    panX += (1 - growth) * (clientX - (shown.left + shown.width / 2));
    panY += (1 - growth) * (clientY - (shown.top + shown.height / 2));
    scale = closer;
    hold();
  }

  function zoomAboutTheMiddle(by) {
    const box = stage().getBoundingClientRect();
    zoomAbout(by, box.left + box.width / 2, box.top + box.height / 2);
  }

  function closerOrBack(clientX, clientY) {
    if (scale > 1) return fit();
    zoomAbout(pixelForPixel(), clientX, clientY);
  }

  // A click is worth one image pixel per screen pixel. A picture already shown that big — the
  // thumbnail standing in for a file we have not fetched — still has to answer the click, so it
  // doubles instead of doing nothing.
  function pixelForPixel() {
    return Math.min(furthestIn, Math.max(2, framed.naturalWidth / framed.offsetWidth || 2));
  }

  function build() {
    const dialog = document.createElement("dialog");
    dialog.className = "viewer";
    dialog.innerHTML =
      '<button type="button" class="viewer-close" data-close aria-label="Close">×</button>' +
      '<button type="button" class="viewer-step viewer-back" data-step="-1" aria-label="Previous">‹</button>' +
      '<button type="button" class="viewer-step viewer-on" data-step="1" aria-label="Next">›</button>' +
      '<figure><div class="viewer-stage"></div><figcaption></figcaption>' +
      '<p class="viewer-counter small"></p></figure>';

    let dragFrom = null;
    let dragged = false;

    dialog.addEventListener("pointerdown", (event) => {
      dragged = false;
      if (scale === 1 || !event.target.closest(".viewer-stage")) return;
      dragFrom = { x: event.clientX, y: event.clientY, panX, panY };
    });

    dialog.addEventListener("pointermove", (event) => {
      if (!dragFrom) return;
      panX = dragFrom.panX + event.clientX - dragFrom.x;
      panY = dragFrom.panY + event.clientY - dragFrom.y;
      dragged = dragged || Math.hypot(event.clientX - dragFrom.x, event.clientY - dragFrom.y) > dragSlop;
      hold();
    });

    const letGo = () => (dragFrom = null);
    dialog.addEventListener("pointerup", letGo);
    dialog.addEventListener("pointercancel", letGo);

    dialog.addEventListener("wheel", (event) => {
      if (!zoomable() || !event.target.closest(".viewer-stage")) return;
      event.preventDefault();
      // A wheel reports its turn in pixels, lines or pages depending on the browser and the
      // device; the two that are not pixels are worth about this much of one.
      const pixels = event.deltaY * ({ 1: 16, 2: 400 }[event.deltaMode] || 1);
      zoomAbout(Math.exp(-pixels / 400), event.clientX, event.clientY);
    }, { passive: false });

    dialog.addEventListener("click", (event) => {
      // The end of a pan is a click as far as the browser is concerned, and closing the viewer
      // because somebody dragged the photograph would be a poor answer to the gesture.
      if (dragged) {
        dragged = false;
        return;
      }
      const stepper = event.target.closest("[data-step]");
      if (stepper) return step(Number(stepper.dataset.step));
      if (event.target === framed) return closerOrBack(event.clientX, event.clientY);
      // Anywhere that is not the photo itself closes: the backdrop is the biggest, most obvious
      // target on the screen and every viewer ever made has closed on it.
      if (!event.target.closest("figure") || event.target.closest("[data-close]")) dialog.close();
    });

    // Escape closes the dialog without asking anyone, and a video left in the stage would go on
    // playing behind the page it is no longer part of.
    dialog.addEventListener("close", () => {
      dialog.querySelector(".viewer-stage").replaceChildren();
      framed = null;
      fit();
    });

    dialog.addEventListener("keydown", (event) => {
      if (event.key === "ArrowRight") step(1);
      if (event.key === "ArrowLeft") step(-1);
      if (event.key === "+" || event.key === "=") zoomAboutTheMiddle(1.25);
      if (event.key === "-") zoomAboutTheMiddle(1 / 1.25);
      if (event.key === "0") fit();
    });

    document.body.append(dialog);
    return dialog;
  }
})();
