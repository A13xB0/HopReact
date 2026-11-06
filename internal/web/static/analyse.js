// Hover readout for the battery analysis chart: the time, the phase of the
// day, the nearest reading and the weather that hour.
(function () {
  "use strict";
  var wrap = document.getElementById("analyse-chart");
  var tip = document.getElementById("analyse-tip");
  if (!wrap || !tip) return;
  var svg = wrap.querySelector(".ac-lg svg");
  if (!svg) return;
  var d;
  try { d = JSON.parse(wrap.getAttribute("data-hover")); } catch (e) { return; }
  var cross = svg.querySelector(".ac-cross");
  var days = ["Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"];

  function offset(t) {
    var o = 0;
    (d.TZOffsets || []).forEach(function (z) { if (t >= z[0]) o = z[1]; });
    return o;
  }
  function stamp(t) {
    var l = new Date((t + offset(t)) * 1000);
    var p = function (n) { return (n < 10 ? "0" : "") + n; };
    return days[l.getUTCDay()] + " " + l.getUTCDate() + " " + p(l.getUTCHours()) + ":" + p(l.getUTCMinutes());
  }
  function nearest(list, t, maxGap) {
    var best = null, gap = Infinity;
    (list || []).forEach(function (x) {
      var g = Math.abs(x[0] - t);
      if (g < gap) { gap = g; best = x; }
    });
    return gap <= maxGap ? best : null;
  }
  function text(parent, s, bold) {
    var el = document.createElement(bold ? "b" : "span");
    el.textContent = s;
    parent.appendChild(el);
    parent.appendChild(document.createElement("br"));
  }

  function move(ev) {
    var r = svg.getBoundingClientRect();
    var sx = (ev.clientX - r.left) / r.width * d.W;
    if (sx < d.L || sx > d.R) { leave(); return; }
    var t = d.T0 + (sx - d.L) / (d.R - d.L) * (d.T1 - d.T0);
    var phase = "";
    (d.Spans || []).forEach(function (s) { if (t >= s.S && t < s.E) phase = s.Phase; });
    tip.textContent = "";
    text(tip, stamp(t) + (phase ? " · " + phase : ""), true);
    var rd = nearest(d.Readings, t, 3 * 3600);
    text(tip, rd ? (rd[1] / 1000).toFixed(3) + " V, read " + stamp(rd[0]).slice(-5) : "No reading near this time");
    // Weather hours are stamped at their end.
    var h = nearest((d.Hours || []).map(function (x) { return [x[0] - 1800, x[1], x[2], x[3]]; }), t, 3600);
    if (h) text(tip, "Sun " + h[1] + " W/m² · cloud " + h[2] + "% · air " + h[3] + " °C");
    var tc = nearest(d.Temps, t, 3 * 3600);
    if (tc) text(tip, "Repeater " + Math.round(tc[1]) + " °C");
    tip.hidden = false;
    var wr = wrap.getBoundingClientRect();
    var px = ev.clientX - wr.left + 12;
    tip.style.left = Math.max(0, Math.min(px, wr.width - tip.offsetWidth - 4)) + "px";
    if (cross) {
      cross.setAttribute("x1", sx); cross.setAttribute("x2", sx);
      cross.setAttribute("visibility", "visible");
    }
  }
  function leave() {
    tip.hidden = true;
    if (cross) cross.setAttribute("visibility", "hidden");
  }
  svg.addEventListener("pointermove", move);
  svg.addEventListener("pointerleave", leave);
})();
