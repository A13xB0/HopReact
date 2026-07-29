// Group buttons tick every checkbox in their group.
//
// Served as a file rather than inlined in the page: the Content-Security-
// Policy is default-src 'self' with no 'unsafe-inline' for scripts, so an
// inline <script> is silently blocked and the buttons do nothing. Keeping
// the policy strict and the script external is the right way round.
//
// Progressive enhancement only. Every checkbox is still there to tick by
// hand, so the form works with this file blocked, failed or disabled.
(function () {
  "use strict";
  // A checkbox that saves itself. The button beside it is the no-JavaScript
  // path and is removed once we know we can submit for them, so there is
  // never a stale-looking Save sitting next to a control that already saved.
  document.querySelectorAll("[data-autosubmit]").forEach(function (input) {
    input.addEventListener("change", function () {
      if (input.form) input.form.submit();
    });
  });
  document.querySelectorAll("[data-hide-when-scripted]").forEach(function (el) {
    el.hidden = true;
  });

  document.querySelectorAll(".chip[data-group]").forEach(function (btn) {
    btn.addEventListener("click", function () {
      var body = document.querySelector(
        '[data-group-body="' + btn.dataset.group + '"]'
      );
      if (!body) return;
      var boxes = body.querySelectorAll("input[type=checkbox]");
      // Tick all, unless they are already all ticked — then clear them, so
      // the button is a toggle rather than a one-way switch.
      var turnOn = Array.prototype.some.call(boxes, function (b) {
        return !b.checked;
      });
      Array.prototype.forEach.call(boxes, function (b) {
        b.checked = turnOn;
      });
      btn.classList.toggle("on", turnOn);
    });
  });
})();
