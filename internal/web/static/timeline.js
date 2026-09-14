// The grid as one piece. A page of two hundred cells with a pager is what a browser without
// script gets, and what this starts from; with script the page becomes the whole grid — fifty
// thousand cells on the photo page — laid out from the month counts the server put on the grid
// element, so the browser's own scrollbar spans the library before any of it has been fetched.
// Only the rows near the viewport exist in the document; the cells for them arrive in windows
// of a page each, through the same template the page itself was drawn with.
//
// Beside the grid, while it is on screen, hangs a rail: the years down the right edge, a mark
// for where the viewport is, and a drag along it that lands on any month. Thumbnails cost one
// throttled Google request each, so nothing is fetched while the rail is being dragged or the
// page is still moving — the rows show as grey cells and the month label says where you are —
// and the pictures come once the page has stopped.
(function () {
  const settleDelay = 150;
  const keptWindows = 40;
  const overscan = 1.0;
  const nearby = 0.25;
  const labelClearance = 22;
  const monthNames = ["January", "February", "March", "April", "May", "June", "July", "August",
    "September", "October", "November", "December"];

  wireGrids();
  document.addEventListener("gpb:swapped", wireGrids);

  function wireGrids() {
    const grid = document.getElementById("grid");
    if (!grid || !grid.dataset.cells || grid.dataset.timelineWired) return;
    grid.dataset.timelineWired = "true";
    timeline(grid);
  }

  function timeline(grid) {
    const total = Number(grid.dataset.total);
    const windowSize = Number(grid.dataset.window);
    const cellsURL = grid.dataset.cells;
    const bands = JSON.parse(grid.dataset.months);
    if (!total || bands.length === 0) return;

    let start = 0;
    bands.forEach((band) => {
      band.start = start;
      band.name = nameOf(band.month);
      start += band.count;
    });

    // The windows of cells that have been fetched, by window number, each a promise of the
    // cell elements in it. The page's own cells are the first window held.
    const windows = new Map();
    const pageCells = Array.from(grid.querySelectorAll(".grid-cell"));
    const pageOffset = Number(grid.dataset.offset);
    if (pageCells.length > 0 && pageOffset % windowSize === 0) {
      keep(pageOffset / windowSize, pageCells);
    }

    document.querySelectorAll("nav.pager").forEach((pager) => pager.remove());
    grid.classList.add("timeline");
    parkImages(grid);
    grid.replaceChildren();

    const now = document.createElement("div");
    now.className = "timeline-now";
    now.setAttribute("aria-hidden", "true");
    grid.append(now);

    const rail = buildRail();
    document.body.append(rail.element);

    let rows = [];
    let columns = 0;
    let metrics = null;
    let laidOutAt = 0;
    let mounted = new Map();
    let settleTimer = null;
    let dragging = false;
    let frame = 0;

    layout();
    render();
    if (pageOffset > 0) scrollToIndex(pageOffset);
    fetchPending();

    window.addEventListener("scroll", scheduleRender, { passive: true });
    // The grid's width is what decides the columns, and it changes with the window, with the
    // rail's reserved edge, and with anything above it that the socket swaps — so it is the grid
    // that is watched, not the window.
    const watcher = new ResizeObserver(() => {
      if (grid.offsetWidth !== laidOutAt) relayout();
    });
    watcher.observe(grid);

    grid.gpbTimeline = {
      length: total,
      at: cellAt,
      indexOf: (cell) => Number(cell.dataset.index),
      cells: cachedCells,
    };

    // ---- layout: months and counts become rows with positions ----

    function measure() {
      const probeRow = document.createElement("div");
      probeRow.className = "grid timeline-row";
      probeRow.append(blankCell());
      const probeHead = document.createElement("h2");
      probeHead.className = "timeline-month";
      probeHead.textContent = "Probe";
      grid.append(probeHead, probeRow);

      const style = getComputedStyle(probeRow);
      const measured = {
        columns: style.gridTemplateColumns.split(" ").length,
        cellHeight: probeRow.offsetHeight,
        gap: parseFloat(style.rowGap) || 0,
        headHeight: probeHead.offsetHeight,
      };
      probeHead.remove();
      probeRow.remove();
      return measured;
    }

    function layout() {
      laidOutAt = grid.offsetWidth;
      metrics = measure();
      columns = Math.max(1, metrics.columns);
      rows = [];
      let top = 0;
      bands.forEach((band) => {
        rows.push({ kind: "head", band, top, height: metrics.headHeight });
        top += metrics.headHeight + metrics.gap;
        for (let first = 0; first < band.count; first += columns) {
          const count = Math.min(columns, band.count - first);
          rows.push({ kind: "cells", band, top, height: metrics.cellHeight, first: band.start + first, count });
          top += metrics.cellHeight + metrics.gap;
        }
      });
      grid.style.height = `${top}px`;
    }

    function relayout() {
      if (!grid.isConnected) return teardown();
      const anchor = indexAtTop();
      layout();
      scrollToIndex(anchor);
      render();
      scheduleFetch();
    }

    // ---- rendering: only the rows near the viewport are in the document ----

    function scheduleRender() {
      if (frame) return;
      frame = requestAnimationFrame(() => {
        frame = 0;
        render();
        scheduleFetch();
      });
    }

    function gridTop() {
      return grid.getBoundingClientRect().top + window.scrollY;
    }

    function render() {
      if (!grid.isConnected) return teardown();

      const viewTop = window.scrollY - gridTop();
      const from = viewTop - window.innerHeight * overscan;
      const to = viewTop + window.innerHeight * (1 + overscan);

      const wanted = new Set();
      for (let index = firstRowBelow(from); index < rows.length && rows[index].top <= to; index++) {
        wanted.add(index);
        if (!mounted.has(index) || arrived(index)) mount(index);
        if (isNear(rows[index], viewTop)) resumeImages(mounted.get(index));
      }
      mounted.forEach((element, index) => {
        if (wanted.has(index)) return;
        parkImages(element);
        element.remove();
        mounted.delete(index);
      });

      // The floating month is for a reader deep in the grid; at the top it would only sit on
      // the first month's own heading and say the same thing twice.
      const shown = bandAt(viewTop);
      now.textContent = shown ? shown.name : "";
      now.hidden = !shown || viewTop <= 0;
      rail.follow();
    }

    function arrived(index) {
      const element = mounted.get(index);
      const row = rows[index];
      return element.dataset.pending && fetchedCells(row.first, row.count) !== null;
    }

    function firstRowBelow(position) {
      let low = 0;
      let high = rows.length;
      while (low < high) {
        const middle = (low + high) >> 1;
        if (rows[middle].top + rows[middle].height < position) low = middle + 1;
        else high = middle;
      }
      return low;
    }

    function mount(index) {
      const row = rows[index];
      const element = row.kind === "head" ? headFor(row) : rowFor(row);
      element.style.top = `${row.top}px`;
      const previous = mounted.get(index);
      if (previous) previous.replaceWith(element);
      else grid.append(element);
      mounted.set(index, element);
    }

    function headFor(row) {
      const head = document.createElement("h2");
      head.className = "timeline-month";
      head.textContent = row.band.name;
      return head;
    }

    function rowFor(row) {
      const element = document.createElement("div");
      element.className = "grid timeline-row";
      const cells = fetchedCells(row.first, row.count);
      if (cells) {
        cells.forEach(parkImages);
        element.append(...cells);
      } else {
        for (let i = 0; i < row.count; i++) element.append(blankCell());
        element.dataset.pending = "true";
      }
      return element;
    }

    // A cell's thumbnail is a throttled request to Google, and the browser goes on loading an
    // image after its element has left the document. Five jumps along the rail would queue five
    // screens of pictures nobody is looking at ahead of the one they are — so a row that leaves
    // the document lets go of what it was still waiting for, and asks again if it comes back.
    // The rows mounted above and below the viewport wait the same way: only the rows near it
    // ask for their pictures, so what is on screen is never sharing the budget with what is not.
    function isNear(row, viewTop) {
      return row.top + row.height >= viewTop - window.innerHeight * nearby &&
        row.top <= viewTop + window.innerHeight * (1 + nearby);
    }

    function parkImages(element) {
      element.querySelectorAll("img.thumb[src]").forEach((image) => {
        // An image the browser was never asked for reports itself complete, so what it holds
        // is the test, not whether it is done.
        if (image.complete && image.naturalWidth > 0) return;
        image.dataset.src = image.getAttribute("src");
        image.removeAttribute("src");
      });
    }

    function resumeImages(element) {
      element.querySelectorAll("img.thumb[data-src]").forEach((image) => {
        image.setAttribute("src", image.dataset.src);
        delete image.dataset.src;
      });
    }

    function blankCell() {
      const figure = document.createElement("figure");
      figure.className = "grid-cell grid-cell-blank";
      figure.innerHTML = '<span class="thumb loading"></span><figcaption>&nbsp;</figcaption>';
      return figure;
    }

    // ---- fetching: windows of cells, only once the page has stopped moving ----

    function scheduleFetch() {
      clearTimeout(settleTimer);
      settleTimer = setTimeout(fetchPending, settleDelay);
    }

    function fetchPending() {
      if (dragging || !grid.isConnected) return;
      const needed = new Set();
      mounted.forEach((element, index) => {
        if (!element.dataset.pending) return;
        const row = rows[index];
        for (let i = row.first; i < row.first + row.count; i += windowSize) needed.add(Math.floor(i / windowSize));
        needed.add(Math.floor((row.first + row.count - 1) / windowSize));
      });
      needed.forEach((number) => fetchWindow(number).then(render, render));
      forget(needed);
    }

    function fetchWindow(number) {
      if (windows.has(number)) return windows.get(number).promise;
      const offset = number * windowSize;
      const promise = fetch(`${cellsURL}?offset=${offset}&limit=${windowSize}`)
        .then((response) => {
          if (response.status === 401) return window.gpb.signInAgain(), [];
          if (!response.ok) throw new Error(`HTTP ${response.status}`);
          return response.text();
        })
        .then((html) => {
          const cells = html ? parseCells(html) : [];
          keep(number, cells);
          return cells;
        })
        .catch((error) => {
          windows.delete(number);
          throw error;
        });
      windows.set(number, { promise, cells: null, number });
      return promise;
    }

    function parseCells(html) {
      const holder = document.createElement("template");
      holder.innerHTML = html;
      return Array.from(holder.content.querySelectorAll(".grid-cell"));
    }

    function keep(number, cells) {
      cells.forEach((cell, i) => (cell.dataset.index = number * windowSize + i));
      windows.set(number, { promise: Promise.resolve(cells), cells, number });
      grid.dispatchEvent(new CustomEvent("gpb:cells", { detail: { cells } }));
    }

    function fetchedCells(first, count) {
      const cells = [];
      for (let i = first; i < first + count; i++) {
        const held = windows.get(Math.floor(i / windowSize));
        if (!held || !held.cells) return null;
        const cell = held.cells[i % windowSize];
        if (!cell) return null;
        cells.push(cell);
      }
      return cells;
    }

    // Windows far from the viewport are let go, keeping the document's memory bounded on a
    // long browse; the page's own window is never held onto for any longer than the others.
    function forget(keeping) {
      if (windows.size <= keptWindows) return;
      const here = indexAtTop() / windowSize;
      Array.from(windows.values())
        .filter((held) => !keeping.has(held.number))
        .sort((a, b) => Math.abs(b.number - here) - Math.abs(a.number - here))
        .slice(0, windows.size - keptWindows)
        .forEach((held) => windows.delete(held.number));
    }

    function cellAt(index) {
      const number = Math.floor(index / windowSize);
      return fetchWindow(number).then((cells) => cells[index % windowSize]);
    }

    function* cachedCells() {
      for (const held of windows.values()) if (held.cells) yield* held.cells;
    }

    // ---- where things are ----

    function bandAt(position) {
      const index = firstRowBelow(position);
      return index < rows.length ? rows[index].band : bands[bands.length - 1];
    }

    function indexAtTop() {
      const index = firstRowBelow(window.scrollY - gridTop());
      for (let i = index; i < rows.length; i++) if (rows[i].kind === "cells") return rows[i].first;
      return 0;
    }

    function rowOfIndex(itemIndex) {
      let low = 0;
      let high = rows.length;
      while (low < high) {
        const middle = (low + high) >> 1;
        const row = rows[middle];
        const last = row.kind === "cells" ? row.first + row.count - 1 : row.band.start - 1;
        if (last < itemIndex) low = middle + 1;
        else high = middle;
      }
      return Math.min(low, rows.length - 1);
    }

    function scrollToIndex(itemIndex) {
      const row = rows[rowOfIndex(itemIndex)];
      // Land on the month header when the item is the first of its month, so a jump reads as
      // "here is August" rather than a row of August with its name just off the top.
      const target = row.kind === "cells" && row.first === row.band.start ? rows[rowOfIndex(itemIndex) - 1] : row;
      window.scrollTo({ top: gridTop() + target.top, behavior: "instant" });
    }

    // ---- the rail ----

    function buildRail() {
      const element = document.createElement("div");
      element.className = "rail";
      element.hidden = true;
      element.innerHTML =
        '<div class="rail-track"><div class="rail-mark"></div>' +
        '<div class="rail-label" hidden></div></div>';
      const track = element.querySelector(".rail-track");
      const mark = element.querySelector(".rail-mark");
      const label = element.querySelector(".rail-label");

      function fractionOf(itemIndex) {
        return total > 1 ? itemIndex / (total - 1) : 0;
      }

      // Years down the track, each where its first item is, skipping any that would sit on
      // top of the one before it. Reads the grid's own order, so the photo page runs down from
      // this year and an album runs down towards it.
      function place() {
        track.querySelectorAll(".rail-year").forEach((year) => year.remove());
        const height = track.clientHeight;
        let lastYear = null;
        let lastAt = -Infinity;
        bands.forEach((band) => {
          const year = band.month ? band.month.slice(0, 4) : "";
          if (year === lastYear || !year) return;
          const at = fractionOf(band.start) * height;
          if (at - lastAt < labelClearance) return;
          const tick = document.createElement("span");
          tick.className = "rail-year";
          tick.textContent = year;
          tick.style.top = `${at}px`;
          track.append(tick);
          lastYear = year;
          lastAt = at;
        });
      }

      let placedFor = 0;

      function follow() {
        const box = grid.getBoundingClientRect();
        const onScreen = box.bottom > 0 && box.top < window.innerHeight;
        const worthIt = box.height > window.innerHeight * 1.5;
        element.hidden = !(onScreen && worthIt);
        if (element.hidden) return;
        // The years can only be spaced along a track that has a height, which a hidden one
        // does not, so they are laid out the first time the rail shows and whenever it grows.
        if (track.clientHeight !== placedFor) {
          placedFor = track.clientHeight;
          place();
        }
        mark.style.top = `${fractionOf(indexAtTop()) * track.clientHeight}px`;
      }

      function indexUnder(clientY) {
        const box = track.getBoundingClientRect();
        const fraction = Math.min(1, Math.max(0, (clientY - box.top) / box.height));
        return Math.round(fraction * (total - 1));
      }

      function say(itemIndex, clientY) {
        const box = track.getBoundingClientRect();
        label.textContent = bandOfIndex(itemIndex).name;
        label.style.top = `${Math.min(box.height, Math.max(0, clientY - box.top))}px`;
        label.hidden = false;
      }

      element.addEventListener("pointerdown", (event) => {
        event.preventDefault();
        element.setPointerCapture(event.pointerId);
        dragging = true;
        element.classList.add("rail-dragging");
        const itemIndex = indexUnder(event.clientY);
        scrollToIndex(itemIndex);
        say(itemIndex, event.clientY);
      });

      element.addEventListener("pointermove", (event) => {
        const itemIndex = indexUnder(event.clientY);
        say(itemIndex, event.clientY);
        if (dragging) scrollToIndex(itemIndex);
      });

      const letGo = () => {
        if (!dragging) return;
        dragging = false;
        element.classList.remove("rail-dragging");
        scheduleFetch();
      };
      element.addEventListener("pointerup", letGo);
      element.addEventListener("pointercancel", letGo);
      element.addEventListener("pointerleave", () => {
        if (!dragging) label.hidden = true;
      });

      return { element, place, follow };
    }

    function bandOfIndex(itemIndex) {
      let low = 0;
      let high = bands.length - 1;
      while (low < high) {
        const middle = (low + high + 1) >> 1;
        if (bands[middle].start <= itemIndex) low = middle;
        else high = middle - 1;
      }
      return bands[low];
    }

    function teardown() {
      window.removeEventListener("scroll", scheduleRender);
      watcher.disconnect();
      rail.element.remove();
    }
  }

  function nameOf(month) {
    if (!month) return "No date";
    return `${monthNames[Number(month.slice(5, 7)) - 1]} ${month.slice(0, 4)}`;
  }
})();
