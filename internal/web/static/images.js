// Every picture on a page comes from Google one throttled request at a time, so a grid of 200
// cells fills over several seconds and an album list of 199 covers longer still. The cells
// shimmer while they wait; this is what stops each one when its picture lands.
//
// Without this file the shimmer simply never stops — it is hidden behind the loaded image, so
// the page is still correct, just needlessly busy. Nothing here is required for the page to work.
(function () {
  const settle = (image) => image.classList.remove("loading");

  // Anything already in the browser cache finished before this script ran and fires no event.
  // Markup swapped in later can carry cached pictures too, which is the same silence.
  settleWhatIsAlreadyHere();
  document.addEventListener("gpb:swapped", settleWhatIsAlreadyHere);

  function settleWhatIsAlreadyHere() {
    document.querySelectorAll("img.thumb").forEach((image) => {
      if (image.complete) settle(image);
    });
  }

  // Capture, because load and error do not bubble. One listener covers images the page adds
  // later, which lazy loading makes routine.
  ["load", "error"].forEach((event) =>
    document.addEventListener(
      event,
      (fired) => {
        const image = fired.target;
        if (image instanceof HTMLImageElement && image.classList.contains("thumb")) settle(image);
      },
      true,
    ),
  );
})();
