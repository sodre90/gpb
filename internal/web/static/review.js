// The queue arrives with every box ticked, which is what makes the no-script browser workable:
// untick the few you disagree with and press a button. That is a poor gesture for a group of two
// hundred, and these two buttons are all this file adds. They sit hidden in the markup because
// without this script they would be two buttons that do nothing.
(function () {
  document.querySelectorAll(".grid-ticktools").forEach((tools) => {
    const group = tools.closest("form");
    if (!group) return;
    tools.hidden = false;

    tools.addEventListener("click", (event) => {
      const button = event.target.closest("button[data-ticked]");
      if (!button) return;

      const ticked = button.dataset.ticked === "true";
      group.querySelectorAll("input[type=checkbox]").forEach((box) => {
        box.checked = ticked;
      });
    });
  });
})();
