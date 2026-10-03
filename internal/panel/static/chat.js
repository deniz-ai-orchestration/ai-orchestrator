// Agent page: follow the running turn's transcript, keep the conversation
// scrolled to the end, and clear the form after a message is sent.
(function () {
  var live = document.getElementById("live");
  var current = "0";
  var es = null;

  function follow() {
    var head = document.getElementById("agent-head");
    var run = head ? head.dataset.run : "0";
    if (run === current) return;
    current = run;
    if (es) { es.close(); es = null; }
    if (run === "0") return;
    live.textContent = "";
    es = new EventSource("/runs/" + run + "/stream");
    es.onmessage = function (e) {
      var atEnd = live.scrollTop + live.clientHeight >= live.scrollHeight - 4;
      live.appendChild(document.createTextNode(e.data + "\n"));
      if (atEnd) live.scrollTop = live.scrollHeight;
    };
    es.addEventListener("end", function (e) {
      es.close(); es = null;
      live.appendChild(document.createTextNode("-- turn " + e.data + " --\n"));
      document.body.dispatchEvent(new Event("refresh"));
    });
    es.onerror = function () { if (es) { es.close(); es = null; } };
  }

  function toEnd() { window.scrollTo(0, document.body.scrollHeight); }

  document.body.addEventListener("htmx:afterSwap", function (evt) {
    if (evt.detail.target.id === "agent-head") follow();
  });
  document.body.addEventListener("sent", function () {
    var f = document.querySelector("form.send");
    if (f) f.reset();
    setTimeout(toEnd, 300);
  });
  follow();
  toEnd();
})();
