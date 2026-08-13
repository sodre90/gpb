// Shared by the two pages that save without reloading. A failed save and an expired session
// look the same from either of them, and a banner that appeared somewhere else or said
// something else on each page would be two behaviours for the user to learn instead of one.
(function () {
  window.gpb = window.gpb || {};

  // An expired web session answers every save with a bare 401. Reporting that verbatim tells
  // the user a number; sending them to the login page and back tells them what to do.
  window.gpb.signInAgain = function () {
    window.location.assign(`/login?next=${encodeURIComponent(window.location.pathname)}`);
  };

  // The banner is made on first need and reused after, so a run of failures leaves one message
  // rather than a column of identical ones. The anchor is resolved late: on the album grid it
  // is an element the page script owns.
  window.gpb.bannerIn = function (id, anchor) {
    return function (message) {
      let banner = document.getElementById(id);
      if (!banner) {
        banner = document.createElement("p");
        banner.id = id;
        banner.className = "banner danger";
        banner.setAttribute("role", "alert");
        anchor().after(banner);
      }
      banner.textContent = message;
    };
  };
})();
