// Keep a section from refreshing while one of its selectors is open, so a
// poll does not reset what you are choosing.
document.addEventListener("htmx:beforeRequest", function (evt) {
  var a = document.activeElement;
  if (evt.detail.requestConfig.verb === "get" && a && a.tagName === "SELECT" && evt.detail.elt.contains(a)) {
    evt.preventDefault();
  }
});
