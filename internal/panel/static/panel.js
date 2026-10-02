// Keep a section from refreshing while one of its selectors is open, so a
// poll does not reset what you are choosing.
document.addEventListener("htmx:beforeRequest", function (evt) {
  var a = document.activeElement;
  if (evt.detail.requestConfig.verb === "get" && a && a.tagName === "SELECT" && evt.detail.elt.contains(a)) {
    evt.preventDefault();
  }
});

// Role model picker: choosing a provider shows only its models and selects
// the first one; "auto" goes back to the role's fallback order.
document.addEventListener("change", function (evt) {
  var prov = evt.target;
  if (!prov.classList || !prov.classList.contains("provider")) return;
  var model = prov.form.querySelector("select.model");
  var first = null;
  for (var i = 0; i < model.options.length; i++) {
    var o = model.options[i];
    o.hidden = o.dataset.provider !== prov.value;
    if (!o.hidden && first === null) first = o;
  }
  if (first) model.value = first.value;
});
