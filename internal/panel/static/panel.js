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

// Summon form: picking a role preselects its model and that model's
// provider.
function applyRole(role) {
  var form = role.form;
  var want = role.selectedOptions[0] && role.selectedOptions[0].dataset.model;
  var model = form.querySelector("select.model");
  var prov = form.querySelector("select.provider");
  var opt = want && model.querySelector('option[value="' + CSS.escape(want) + '"]');
  if (!opt) opt = model.options[0];
  if (!opt) return;
  prov.value = opt.dataset.provider;
  for (var i = 0; i < model.options.length; i++) {
    model.options[i].hidden = model.options[i].dataset.provider !== prov.value;
  }
  model.value = opt.value;
}
document.addEventListener("change", function (evt) {
  if (evt.target.classList && evt.target.classList.contains("role")) applyRole(evt.target);
});
document.addEventListener("DOMContentLoaded", function () {
  document.querySelectorAll("select.role").forEach(applyRole);
});
