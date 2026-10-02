// Live, view-only transcript of one run.
(function () {
  var el = document.getElementById("terminal");
  var term = new Terminal({
    disableStdin: true,
    convertEol: true,
    scrollback: 20000,
    fontSize: 13,
    fontFamily: "ui-monospace, SFMono-Regular, Menlo, Consolas, monospace",
    theme: { background: "#111418", foreground: "#d8dee9" },
  });
  var fit = new FitAddon.FitAddon();
  term.loadAddon(fit);
  term.open(el);
  fit.fit();
  window.addEventListener("resize", function () { fit.fit(); });

  var colors = [
    ["▸", "\x1b[36m"],     // tool call
    ["  ↳", "\x1b[2m"],    // tool result
    ["!", "\x1b[31m"],     // error
    ["■", "\x1b[1m"],      // end of turn
    ["●", "\x1b[1m"],      // session start
    ["stderr:", "\x1b[33m"],
  ];
  function color(line) {
    for (var i = 0; i < colors.length; i++) {
      if (line.indexOf(colors[i][0]) === 0) return colors[i][1] + line + "\x1b[0m";
    }
    return line;
  }

  var es = new EventSource(el.dataset.stream);
  es.onmessage = function (e) { term.writeln(color(e.data)); };
  es.addEventListener("end", function (e) {
    es.close();
    term.writeln("\x1b[1m-- run " + e.data + " --\x1b[0m");
    var s = document.getElementById("state");
    s.textContent = e.data;
    s.className = "badge " + e.data.split(" ")[0];
  });
  es.onerror = function () {
    // EventSource would reconnect and replay from the start; stop instead.
    if (es.readyState !== EventSource.CLOSED) {
      es.close();
      term.writeln("\x1b[33m-- connection lost; reload to resume --\x1b[0m");
    }
  };
})();
