// Behaviour the pages need beyond plain forms. Everything degrades to
// working forms and server-rendered pages when scripts are off; nothing
// here talks to anything but this server.
(function () {
  "use strict";

  // Theme: the server renders data-theme from ui.theme (dark unless set
  // otherwise). The header toggle keeps a choice per browser; if storage is
  // unavailable the server default stays.
  var themeKey = "composewright.theme";
  var themes = ["dark", "light", "system"];
  var root = document.documentElement;
  var secondsPerMinute = 60;
  var secondsPerHour = 3600;
  var tickMilliseconds = 1000;
  // The server pings a run's stream every 15 seconds (sseHeartbeat); three
  // missed pings mean the connection is gone.
  var silenceMilliseconds = 45000;
  var linesAboveSelection = 2;

  // Preferences kept per browser: the theme and the Result page's layout.
  // Storage can be off (private windows); the defaults stay then.
  function stored(key) {
    try {
      return window.localStorage.getItem(key);
    } catch (error) {
      return null;
    }
  }

  function store(key, value) {
    try {
      window.localStorage.setItem(key, value);
    } catch (error) {
      return;
    }
  }

  function storedTheme() {
    return stored(themeKey);
  }

  function storeTheme(theme) {
    store(themeKey, theme);
  }

  function applyTheme(theme) {
    if (themes.indexOf(theme) < 0) {
      return;
    }
    root.setAttribute("data-theme", theme);
  }

  function effectiveTheme() {
    var theme = root.getAttribute("data-theme");
    if (theme !== "system") {
      return theme;
    }
    var prefersLight = window.matchMedia && window.matchMedia("(prefers-color-scheme: light)").matches;
    return prefersLight ? "light" : "dark";
  }

  function describeToggle(toggle) {
    var next = effectiveTheme() === "light" ? "dark" : "light";
    toggle.setAttribute("aria-label", "Switch to the " + next + " theme");
    toggle.setAttribute("aria-pressed", effectiveTheme() === "light" ? "true" : "false");
  }

  function setUpToggle() {
    var toggle = document.getElementById("theme-toggle");
    if (!toggle) {
      return;
    }
    describeToggle(toggle);
    toggle.addEventListener("click", function () {
      var next = effectiveTheme() === "light" ? "dark" : "light";
      applyTheme(next);
      storeTheme(next);
      describeToggle(toggle);
    });
  }

  applyTheme(storedTheme());

  // Side by side, on the Result page: the result panel folds into a rail and
  // compose.yaml and .env are edited next to each other. The class goes on
  // before the page paints; the stylesheet keeps it to pages with the editor.
  var layoutKey = "sw.result.layout";
  var sideBySide = "side-by-side";
  var resultPanel = "panel";
  var checkDelay = 400;
  var composeShare = 60;
  var minimumPane = 320;
  var splitterStep = 2;
  var refusedStatus = 422;
  var staleStatus = 409;
  var indentText = "  ";
  var editedMark = { badge: "U", tone: "you" };

  if (stored(layoutKey) === sideBySide) {
    root.classList.add(sideBySide);
  }

  // Copy buttons: the text of an element, or the .env with its secrets,
  // which the server hands over only on a POST it writes to the audit log.
  function confirmCopy(button) {
    var label = button.getAttribute("data-label") || button.textContent;
    button.setAttribute("data-label", label);
    button.textContent = "Copied";
    window.setTimeout(function () {
      button.textContent = label;
    }, 2000);
  }

  function selectText(element) {
    var range = document.createRange();
    range.selectNodeContents(element);
    var selection = window.getSelection();
    selection.removeAllRanges();
    selection.addRange(range);
  }

  // legacyCopy is for browsers that refuse the asynchronous clipboard.
  function legacyCopy(text) {
    var area = document.createElement("textarea");
    area.value = text;
    area.setAttribute("readonly", "");
    area.className = "visually-hidden";
    document.body.appendChild(area);
    area.select();
    var copied = false;
    try {
      copied = document.execCommand("copy");
    } catch (problem) {
      copied = false;
    }
    document.body.removeChild(area);
    return copied;
  }

  // copyText tries the clipboard, then the legacy command, and finally
  // selects the text so Ctrl+C works.
  function copyText(button, text, fallback) {
    var giveUp = function () {
      if (legacyCopy(text)) {
        confirmCopy(button);
        return;
      }
      if (!fallback) {
        button.textContent = "Copy blocked by the browser";
        return;
      }
      fallback.textContent = text;
      selectText(fallback);
      button.textContent = "Press Ctrl+C";
    };
    if (!navigator.clipboard) {
      giveUp();
      return;
    }
    navigator.clipboard.writeText(text).then(function () {
      confirmCopy(button);
    }, giveUp);
  }

  function setUpCopy() {
    document.querySelectorAll("[data-copy-target]").forEach(function (button) {
      button.addEventListener("click", function () {
        var target = document.getElementById(button.getAttribute("data-copy-target"));
        if (target) {
          copyText(button, "value" in target ? target.value : target.textContent, target);
        }
      });
    });
    document.querySelectorAll("[data-copy-env]").forEach(function (button) {
      button.addEventListener("click", function () {
        fetchEnv(button).then(function (text) {
          copyText(button, text, null);
        }, function () {
          button.textContent = "Copy failed";
        });
      });
    });
  }

  // fetchEnv gets what carries secrets, the .env or every file as shell
  // commands; the server writes the copy to the audit log.
  function fetchEnv(button) {
    var url = button.getAttribute("data-copy-env");
    var headers = { "X-CSRF-Token": button.getAttribute("data-csrf") };
    return fetch(url, { method: "POST", headers: headers, credentials: "same-origin" }).then(function (response) {
      if (!response.ok) {
        throw new Error(response.statusText);
      }
      return response.text();
    });
  }

  // Files: drop them anywhere in the window or pick them; list what is
  // chosen.
  function listFiles(input, list) {
    list.textContent = "";
    Array.prototype.forEach.call(input.files, function (file) {
      var item = document.createElement("li");
      item.className = "chip chip-mono";
      item.textContent = file.name + " · " + Math.max(1, Math.round(file.size / 1024)) + " KB";
      list.appendChild(item);
    });
  }

  function setUpDropzone() {
    var zone = document.querySelector("[data-dropzone]");
    var input = document.querySelector("[data-file-input]");
    var list = document.querySelector("[data-file-list]");
    if (!zone || !input || !list) {
      return;
    }
    ["dragenter", "dragover"].forEach(function (name) {
      document.addEventListener(name, function (event) {
        event.preventDefault();
        zone.classList.add("is-over");
      });
    });
    ["dragleave", "drop"].forEach(function (name) {
      document.addEventListener(name, function (event) {
        if (name === "dragleave" && event.relatedTarget) {
          return;
        }
        zone.classList.remove("is-over");
      });
    });
    document.addEventListener("drop", function (event) {
      event.preventDefault();
      if (event.dataTransfer && event.dataTransfer.files.length) {
        input.files = event.dataTransfer.files;
        listFiles(input, list);
      }
    });
    input.addEventListener("change", function () {
      listFiles(input, list);
    });
  }

  // File tabs in the context pane switch without a reload; the links still
  // work without scripts.
  function setUpTabs() {
    document.querySelectorAll("[data-tabs]").forEach(function (tabs) {
      var pane = tabs.parentElement;
      tabs.querySelectorAll("[data-tab]").forEach(function (tab) {
        tab.addEventListener("click", function (event) {
          event.preventDefault();
          var name = tab.getAttribute("data-tab");
          tabs.querySelectorAll("[data-tab]").forEach(function (other) {
            other.setAttribute("aria-selected", other === tab ? "true" : "false");
          });
          pane.querySelectorAll("[data-panel]").forEach(function (panel) {
            panel.hidden = panel.getAttribute("data-panel") !== name;
          });
        });
      });
    });
  }

  // The compose editor on the Review page opens in place of the badged
  // view; Cancel puts the view back without saving.
  // selectLine puts the cursor on a line of a text area, counted from 1,
  // with a little of what comes before it in view; any other number only
  // focuses the text area.
  function selectLine(textarea, line) {
    var lines = textarea.value.split("\n");
    textarea.focus();
    if (!(line >= 1 && line <= lines.length)) {
      return;
    }
    var start = 0;
    for (var index = 0; index < line - 1; index++) {
      start += lines[index].length + 1;
    }
    textarea.setSelectionRange(start, start + lines[line - 1].length);
    var computed = window.getComputedStyle(textarea);
    var height = parseFloat(computed.lineHeight) || parseFloat(computed.fontSize);
    textarea.scrollTop = Math.max(0, (line - 1 - linesAboveSelection) * height);
  }

  // The Edit button opens compose.yaml in the editor; a policy finding's
  // link opens it on the line the finding is about.
  function setUpComposeEditor() {
    var view = document.querySelector("[data-compose-view]");
    var editor = document.querySelector("[data-compose-editor]");
    if (!view || !editor) {
      return;
    }
    var textarea = editor.querySelector("textarea");
    var openAt = function (line) {
      var tab = document.querySelector('[data-tab="compose"]');
      if (tab) {
        tab.click();
      }
      view.hidden = true;
      editor.hidden = false;
      selectLine(textarea, line);
    };
    document.querySelector("[data-edit-compose]").addEventListener("click", function (event) {
      event.preventDefault();
      openAt(0);
    });
    document.querySelectorAll("[data-edit-line]").forEach(function (link) {
      link.addEventListener("click", function (event) {
        event.preventDefault();
        openAt(parseInt(link.getAttribute("data-edit-line"), 10));
      });
    });
    editor.querySelector("[data-edit-cancel]").addEventListener("click", function (event) {
      event.preventDefault();
      editor.hidden = true;
      view.hidden = false;
    });
    var requested = parseInt(new URLSearchParams(window.location.search).get("edit"), 10);
    if (!editor.hidden && requested > 0) {
      selectLine(textarea, requested);
    }
  }

  // Code editors number their lines and keep the numbers beside the text.
  function numberLines(editor) {
    var textarea = editor.querySelector("textarea");
    var numbers = editor.querySelector(".editor-lines");
    var count = Math.max(1, textarea.value.split("\n").length);
    var lines = [];
    for (var line = 1; line <= count; line++) {
      lines.push(line);
    }
    numbers.textContent = lines.join("\n");
    numbers.scrollTop = textarea.scrollTop;
  }

  function setUpEditors() {
    document.querySelectorAll("[data-editor]").forEach(function (editor) {
      var textarea = editor.querySelector("textarea");
      numberLines(editor);
      textarea.addEventListener("input", function () {
        numberLines(editor);
      });
      textarea.addEventListener("scroll", function () {
        editor.querySelector(".editor-lines").scrollTop = textarea.scrollTop;
      });
    });
  }

  // Ctrl+Enter runs the page's main action: the test on the Review page,
  // generating on the Source page.
  function setUpShortcuts() {
    document.addEventListener("keydown", function (event) {
      if (event.key !== "Enter" || !(event.ctrlKey || event.metaKey)) {
        return;
      }
      var run = document.querySelector('[data-shortcut="run"]:not([disabled])');
      var form = document.querySelector("[data-dropzone-form]");
      if (run) {
        event.preventDefault();
        run.click();
        return;
      }
      if (form) {
        event.preventDefault();
        form.requestSubmit();
      }
    });
  }

  // Environment filters only change visibility. Inputs stay enabled and in
  // the form, and search never reads their values (including revealed ones).
  function setUpEnvFilters() {
    var section = document.querySelector("[data-env-review]");
    if (!section) { return; }
    var tools = section.querySelector("[data-env-tools]");
    var search = section.querySelector("[data-env-search-input]");
    var filters = Array.from(section.querySelectorAll('input[name="env-filter"]'));
    var groups = Array.from(section.querySelectorAll("[data-env-group]"));
    var optional = section.querySelector("[data-env-optional]");
    var rows = Array.from(section.querySelectorAll("[data-env-row]")).map(function (row) {
      var input = row.querySelector('input[name^="value:"], input[name^="secret:"]');
      return { element: row, input: input, initial: input ? input.value : "",
        search: row.getAttribute("data-env-search").toLowerCase(),
        optional: row.hasAttribute("data-env-optional-row"),
        secret: row.getAttribute("data-env-secret") === "true",
        required: row.getAttribute("data-env-required") === "true",
        stored: row.getAttribute("data-env-stored") === "true",
        supplied: row.getAttribute("data-env-kind") === "supplied",
        question: row.getAttribute("data-env-question") === "true" };
    });
    function edited(row) { return !!row.input && row.input.value !== row.initial; }
    function needs(row) {
      if (row.optional) { return false; }
      var empty = row.input ? row.input.value === "" && !(row.supplied && row.stored) : !row.stored;
      return ((row.required || row.supplied) && empty) || (row.question && !edited(row));
    }
    function update() {
      var query = search.value.trim().toLowerCase();
      var selected = filters.find(function (filter) { return filter.checked; }).value;
      var counts = { all: 0, needs: 0, optional: 0, secret: 0, changed: 0 };
      var visible = 0;
      var visibleOptional = 0;
      var editing = false;
      rows.forEach(function (row) {
        var flags = { all: true, needs: needs(row), optional: row.optional, secret: row.secret, changed: edited(row) };
        var matches = row.search.indexOf(query) !== -1;
        Object.keys(counts).forEach(function (key) { if (matches && flags[key]) { counts[key]++; } });
        var match = matches && flags[selected];
        var focused = row.element.contains(document.activeElement);
        row.element.hidden = !match && !focused;
        editing = editing || (!match && focused);
        if (match) { visible++; if (row.optional) { visibleOptional++; } }
      });
      groups.forEach(function (group) {
        group.hidden = !Array.from(group.querySelectorAll("[data-env-row]")).some(function (row) { return !row.hidden; });
      });
      tools.querySelectorAll("[data-env-count]").forEach(function (count) {
        count.textContent = "(" + counts[count.getAttribute("data-env-count")] + ")";
      });
      section.querySelector("[data-env-status]").textContent = visible + " of " + rows.length + " settings match." +
        (visible === 0 ? " Clear filters to see every setting." : visibleOptional ? " Optional settings are below." : "") +
        (editing ? " The setting you are editing stays visible until you leave it." : "");
      if (optional) {
        optional.hidden = visibleOptional === 0;
        if (visibleOptional && (query || selected === "optional")) { optional.open = true; }
      }
    }
    function clear() {
      search.value = "";
      filters.forEach(function (filter) { filter.checked = filter.value === "all"; });
      update();
    }
    function revealError(target) {
      var row = target.closest("[data-env-row]");
      if (!row) { return; }
      clear();
      var details = row.closest("details");
      if (details) { details.open = true; }
    }
    search.addEventListener("input", update);
    search.addEventListener("keydown", function (event) { if (event.key === "Enter") { event.preventDefault(); } });
    filters.forEach(function (filter) { filter.addEventListener("change", update); });
    section.addEventListener("input", function (event) { if (event.target.closest("[data-env-row]")) { update(); } });
    section.addEventListener("focusout", function () { window.setTimeout(update, 0); });
    section.querySelector("[data-env-clear]").addEventListener("click", clear);
    section.closest("form").addEventListener("invalid", function (event) { revealError(event.target); }, true);
    // Inline errors added by another enhancement must also escape filters.
    new MutationObserver(function (changes) {
      changes.forEach(function (change) {
        if (change.target.getAttribute("aria-invalid") === "true") { revealError(change.target); }
      });
    }).observe(section, { subtree: true, attributes: true, attributeFilter: ["aria-invalid"] });
    update();
    section.querySelectorAll('[aria-invalid="true"], [data-env-error]').forEach(revealError);
    tools.hidden = false;
    // Following a relationship must reveal its destination even when a
    // search/filter or native disclosure currently hides it.
    function followReference(target) {
      if (!target) { return; }
      var row = target.closest("[data-env-row]");
      if (row) { clear(); }
      for (var ancestor = target; ancestor; ancestor = ancestor.parentElement) {
        if (ancestor.tagName === "DETAILS") { ancestor.open = true; }
      }
      var control = target.querySelector('input:not([type="hidden"])') || target.querySelector("summary");
      if (control) { control.focus(); }
      else { target.tabIndex = -1; target.focus(); }
      target.scrollIntoView({ block: "center" });
    }
    document.querySelectorAll("[data-env-reference]").forEach(function (link) {
      link.addEventListener("click", function () {
        var target = document.getElementById(link.getAttribute("href").slice(1));
        // Reveal before the browser follows the native fragment link.
        followReference(target);
      });
    });
    function followFragment() {
      var id = window.location.hash.slice(1);
      if (id.indexOf("reference-") === 0) { followReference(document.getElementById(id)); }
    }
    window.addEventListener("hashchange", followFragment);
    followFragment();
  }

  // The sidebar filter combines name, status and labels, keeping current navigation.
  function setUpProjectFilter() {
    var collection = document.querySelector("[data-project-collection]");
    if (!collection) { return; }
    var name = collection.querySelector("[data-project-filter]");
    var status = collection.querySelector("select[data-project-status]");
    var label = collection.querySelector("select[data-project-label]");
    var items = Array.from(collection.querySelectorAll("[data-project-item]"));
    var groups = Array.from(collection.querySelectorAll("[data-project-group]"));
    var result = collection.querySelector("[data-project-filter-status]");
    function update() {
      var wanted = name.value.trim().toLowerCase();
      var matches = 0;
      var kept = false;
      items.forEach(function (item) {
        var match = item.getAttribute("data-project-name").toLowerCase().indexOf(wanted) !== -1 &&
          (!status.value || item.getAttribute("data-project-status") === status.value) &&
          (!label.value || item.getAttribute("data-project-labels").split(",").indexOf(label.value) !== -1);
        var current = item.getAttribute("data-project-current") === "true";
        item.hidden = !match && !current;
        if (match) { matches++; }
        kept = kept || (current && !match);
      });
      groups.forEach(function (group) {
        group.hidden = !Array.from(group.querySelectorAll("[data-project-item]")).some(function (item) { return !item.hidden; });
      });
      result.hidden = false;
      result.textContent = matches + " of " + items.length + " projects match." + (kept ? " Your current project stays visible." : "");
    }
    name.addEventListener("input", update);
    status.addEventListener("change", update);
    label.addEventListener("change", update);
    collection.querySelector("[data-project-filter-form]").addEventListener("submit", function (event) { event.preventDefault(); update(); });
    collection.querySelector("[data-project-clear]").addEventListener("click", function (event) {
      event.preventDefault(); name.value = ""; status.value = ""; label.value = ""; update();
    });
    update();
  }

  // Menus close when the person clicks elsewhere.
  function setUpMenus() {
    document.addEventListener("click", function (event) {
      document.querySelectorAll("[data-menu][open]").forEach(function (menu) {
        if (!menu.contains(event.target)) {
          menu.open = false;
        }
      });
    });
  }

  // Runs: steps update in place, sandbox output is appended as it comes,
  // anything else reloads the page, which the server renders from the
  // run's state.
  function followLog(log) {
    var follow = document.querySelector("[data-log-follow]");
    if (!follow || follow.checked) {
      log.scrollTop = log.scrollHeight;
    }
  }

  // The same pattern the server marks kept lines with (views.go).
  var errorLine = /\b(error|fatal|panic|denied|unhealthy|exited with code [1-9])/i;

  function appendLine(log, line) {
    var tone = line.source === "sandbox" || line.source === "compose" ? line.source : "service";
    var row = document.createElement("span");
    row.className = "log-line log-" + tone;
    row.classList.toggle("is-error", errorLine.test(line.text));
    row.title = new Date(line.time).toLocaleTimeString([], { hour12: false });
    var source = document.createElement("span");
    source.className = "log-source";
    source.textContent = line.source + " ";
    var text = document.createElement("span");
    text.className = "log-text";
    text.textContent = line.text + "\n";
    row.appendChild(source);
    row.appendChild(text);
    log.appendChild(row);
    followLog(log);
  }

  // "Add network" copies the blank row kept in a <template>, numbered after
  // the rows already in the table. The placeholder is the server's
  // newRowIndex; the server reads at most data-max rows.
  var rowPlaceholder = "__index__";

  function setUpAddRow() {
    document.querySelectorAll("[data-add-row]").forEach(function (button) {
      var name = button.getAttribute("data-add-row");
      var template = document.getElementById(name);
      var body = document.querySelector('[data-rows="' + name + '"]');
      var limit = parseInt(button.getAttribute("data-max"), 10);
      if (!template || !body) {
        return;
      }
      button.disabled = body.rows.length >= limit;
      button.addEventListener("click", function () {
        var index = String(body.rows.length);
        var row = template.content.firstElementChild.cloneNode(true);
        row.querySelectorAll("[name]").forEach(function (field) {
          field.name = field.name.replace(rowPlaceholder, index);
        });
        body.appendChild(row);
        row.querySelector("input").focus();
        button.disabled = body.rows.length >= limit;
      });
    });
  }

  // The connection form follows the chosen service: its guide, where it
  // lives, a model to start with, its model suggestions and a free name.
  // A name the person typed stays.
  function setUpConnectionForm() {
    var form = document.querySelector("[data-connection-form]");
    if (!form) {
      return;
    }
    var name = form.querySelector('[name="name"]');
    var address = form.querySelector('[name="base_url"]');
    var model = form.querySelector('[name="model"]');
    var budget = form.querySelector('[name="max_input_tokens"]');
    var named = name.readOnly;
    name.addEventListener("input", function () {
      named = true;
    });
    form.querySelectorAll('[name="preset"]').forEach(function (radio) {
      radio.addEventListener("change", function () {
        form.querySelectorAll("[data-guide]").forEach(function (guide) {
          guide.hidden = guide.getAttribute("data-guide") !== radio.value;
        });
        form.querySelectorAll("[data-guide-hint]").forEach(function (hint) {
          hint.hidden = true;
        });
        address.value = radio.getAttribute("data-base-url");
        model.value = radio.getAttribute("data-model");
        budget.value = radio.getAttribute("data-budget");
        model.setAttribute("list", "models-" + radio.value);
        if (!named) {
          name.value = radio.getAttribute("data-name");
        }
      });
    });
  }

  // A settings page is one form: it says when it holds unsaved changes and
  // asks before the browser leaves it with them.
  function setUpUnsaved() {
    var form = document.querySelector("[data-settings-form]");
    if (!form) {
      return;
    }
    var note = document.querySelector("[data-unsaved]");
    var unsaved = Boolean(note && !note.hidden);
    var mark = function () {
      unsaved = true;
      if (note) {
        note.hidden = false;
      }
    };
    form.addEventListener("input", mark);
    form.addEventListener("change", mark);
    form.addEventListener("submit", function () {
      unsaved = false;
    });
    window.addEventListener("beforeunload", function (event) {
      if (unsaved) {
        event.preventDefault();
        event.returnValue = "";
      }
    });
  }

  // A long project list keeps the open project in view.
  function revealCurrentProject() {
    var current = document.querySelector('[data-project-list] [aria-current="page"]');
    if (current) {
      current.scrollIntoView({ block: "nearest" });
    }
  }

  function setUpErrorFilter() {
    var toggle = document.querySelector("[data-errors-only]");
    var log = document.querySelector("[data-log]");
    if (!toggle || !log) {
      return;
    }
    toggle.addEventListener("change", function () {
      log.classList.toggle("log-only-errors", toggle.checked);
    });
  }

  function updateStep(step) {
    var item = document.querySelector('[data-step="' + step.name + '"]');
    if (!item) {
      return false;
    }
    item.setAttribute("data-state", step.state);
    var state = item.querySelector("[data-step-state]");
    if (state) {
      state.textContent = step.state;
    }
    var detail = item.querySelector("[data-step-detail]");
    if (detail && step.note) {
      detail.textContent = step.note;
    }
    return step.state !== "done";
  }

  function setUpJob() {
    var element = document.querySelector("[data-job]");
    if (!element || element.hasAttribute("data-finished") || !window.EventSource) {
      return;
    }
    var kind = element.getAttribute("data-job-kind");
    var log = document.querySelector("[data-log]");
    var stream = new EventSource(element.getAttribute("data-job"));
    // A connection can die without a word: a dropped network, a laptop
    // waking up, too many tabs on one server. When the stream goes silent
    // or closes for good, the page reloads and the server renders where
    // the run is now.
    var silence = 0;
    var listen = function () {
      window.clearTimeout(silence);
      silence = window.setTimeout(function () {
        stream.close();
        window.location.reload();
      }, silenceMilliseconds);
    };
    listen();
    stream.addEventListener("ping", listen);
    stream.onerror = function () {
      if (stream.readyState === EventSource.CLOSED) {
        window.location.reload();
      }
    };
    stream.onmessage = function (message) {
      listen();
      var item = JSON.parse(message.data);
      if (item.kind === "done") {
        stream.close();
        if (!item.failed && item.next) {
          window.location.assign(item.next);
          return;
        }
        window.location.reload();
        return;
      }
      if (item.kind === "line" && log) {
        appendLine(log, item.line);
        return;
      }
      if (item.kind === "step" && kind === "generate") {
        updateStep(item.step);
        return;
      }
      if (item.kind === "step" && kind === "validate") {
        return;
      }
      stream.close();
      window.location.reload();
    };
  }

  // Written like Go's time.Duration, as the server renders it.
  function formatSeconds(total) {
    var hours = Math.floor(total / secondsPerHour);
    var minutes = Math.floor((total % secondsPerHour) / secondsPerMinute);
    var seconds = total % secondsPerMinute;
    if (hours > 0) {
      return hours + "h" + minutes + "m" + seconds + "s";
    }
    if (minutes > 0) {
      return minutes + "m" + seconds + "s";
    }
    return seconds + "s";
  }

  // The server renders the time a running job has taken so far; counting
  // on from there uses no clock but this browser's.
  function tickElapsed() {
    var element = document.querySelector("[data-elapsed]");
    if (!element) {
      return;
    }
    var base = parseInt(element.getAttribute("data-elapsed"), 10) || 0;
    var loaded = Date.now();
    window.setInterval(function () {
      element.textContent = formatSeconds(base + Math.floor((Date.now() - loaded) / tickMilliseconds));
    }, tickMilliseconds);
  }

  // The show-password button keeps its label; aria-pressed says whether the
  // field is shown as plain text.
  function setUpPasswordReveal() {
    document.querySelectorAll("[data-reveal]").forEach(function (button) {
      var field = document.getElementById(button.getAttribute("data-reveal"));
      if (!field) {
        return;
      }
      button.addEventListener("click", function () {
        var shown = field.type === "password";
        field.type = shown ? "text" : "password";
        button.setAttribute("aria-pressed", shown ? "true" : "false");
      });
    });
  }

  // codeEditor is a text area with a gutter (line numbers, a mark on lines
  // changed since the saved version, and compose.yaml's provenance badges)
  // and a layer behind it for the current line, indentation guides and
  // lines a check found wrong. Tab inserts two spaces, since YAML forbids
  // tabs; Escape then Tab leaves the editor. Enter keeps the indentation.
  function codeEditor(container, onChange) {
    var textarea = container.querySelector("textarea");
    var gutter = container.querySelector("[data-gutter]");
    var backdrop = container.querySelector("[data-backdrop]");
    var marks = JSON.parse(container.getAttribute("data-provenance") || "[]");
    var saved = "";
    var savedLines = {};
    var savedMarks = {};
    var problems = {};
    var tabLeaves = false;

    function setBaseline(text) {
      saved = text;
      savedLines = {};
      savedMarks = {};
      text.split("\n").forEach(function (line, index) {
        var key = line.trim();
        savedLines[key] = true;
        if (marks[index] && marks[index].badge && !savedMarks[key]) {
          savedMarks[key] = marks[index];
        }
      });
    }

    function isChanged(line) {
      var key = line.trim();
      return key !== "" && !savedLines[key];
    }

    function changes() {
      var lines = textarea.value.split("\n");
      var present = {};
      lines.forEach(function (line) {
        present[line.trim()] = true;
      });
      var removed = saved.split("\n").filter(function (line) {
        return line.trim() !== "" && !present[line.trim()];
      });
      return lines.filter(isChanged).length + removed.length;
    }

    function caretLine() {
      return textarea.value.slice(0, textarea.selectionStart).split("\n").length;
    }

    function gutterLine(line, number, current) {
      var row = document.createElement("div");
      row.className = "gutter-line";
      row.classList.toggle("is-changed", isChanged(line));
      row.classList.toggle("is-current", number === current);
      var count = document.createElement("span");
      count.className = "gutter-number";
      count.textContent = String(number);
      row.appendChild(count);
      var mark = isChanged(line) ? editedMark : savedMarks[line.trim()];
      if (marks.length === 0 || !mark) {
        return row;
      }
      var badge = document.createElement("span");
      badge.className = "badge badge-" + mark.tone;
      badge.textContent = mark.badge;
      row.appendChild(badge);
      return row;
    }

    function backdropLine(line, number, current) {
      var row = document.createElement("div");
      row.className = "backdrop-line";
      row.classList.toggle("is-current", number === current);
      row.classList.toggle("is-problem", Boolean(problems[number]));
      var depth = Math.floor((line.length - line.replace(/^ +/, "").length) / indentText.length);
      for (var level = 0; level < depth; level++) {
        var guide = document.createElement("span");
        guide.className = "indent-guide";
        row.appendChild(guide);
      }
      return row;
    }

    function follow() {
      gutter.scrollTop = textarea.scrollTop;
      backdrop.style.width = textarea.scrollWidth + "px";
      backdrop.style.transform = "translate(" + -textarea.scrollLeft + "px, " + -textarea.scrollTop + "px)";
    }

    function render() {
      var current = document.activeElement === textarea ? caretLine() : 0;
      var gutterRows = document.createDocumentFragment();
      var backdropRows = document.createDocumentFragment();
      textarea.value.split("\n").forEach(function (line, index) {
        gutterRows.appendChild(gutterLine(line, index + 1, current));
        backdropRows.appendChild(backdropLine(line, index + 1, current));
      });
      gutter.replaceChildren(gutterRows);
      backdrop.replaceChildren(backdropRows);
      follow();
    }

    function insert(text) {
      if (document.execCommand("insertText", false, text)) {
        return;
      }
      textarea.setRangeText(text, textarea.selectionStart, textarea.selectionEnd, "end");
      textarea.dispatchEvent(new Event("input", { bubbles: true }));
    }

    function plainKey(event) {
      return !event.shiftKey && !event.ctrlKey && !event.metaKey && !event.altKey;
    }

    textarea.addEventListener("keydown", function (event) {
      var leaving = tabLeaves;
      tabLeaves = event.key === "Escape";
      if (event.key === "Tab" && plainKey(event) && !leaving) {
        event.preventDefault();
        insert(indentText);
        return;
      }
      if (event.key !== "Enter" || !plainKey(event)) {
        return;
      }
      event.preventDefault();
      var before = textarea.value.slice(0, textarea.selectionStart);
      insert("\n" + before.slice(before.lastIndexOf("\n") + 1).match(/^ */)[0]);
    });
    textarea.addEventListener("input", function () {
      render();
      onChange();
    });
    textarea.addEventListener("scroll", follow);
    ["click", "keyup", "focus", "blur"].forEach(function (name) {
      textarea.addEventListener(name, render);
    });
    setBaseline(textarea.value);
    render();
    return {
      value: function () {
        return textarea.value;
      },
      dirty: function () {
        return textarea.value !== saved;
      },
      changes: changes,
      render: render,
      acceptSaved: function (value) {
        setBaseline(value);
        render();
      },
      reset: function () {
        textarea.value = saved;
        render();
      },
      goToLine: function (number) {
        selectLine(textarea, number);
        render();
      },
      markProblems: function (lines) {
        problems = lines;
        render();
      },
      showProvenance: function (shown) {
        container.classList.toggle("hide-provenance", !shown);
      }
    };
  }

  // The splitter between compose.yaml and .env: drag it, use the arrow keys,
  // or double-click to go back to 60/40. Each side keeps at least 320px.
  function setUpSplitter(panes, splitter) {
    function setShare(percent) {
      panes.style.setProperty("--compose-share", percent + "%");
      splitter.setAttribute("aria-valuenow", String(Math.round(percent)));
    }

    function bounded(percent) {
      var least = Math.min(50, (minimumPane / panes.getBoundingClientRect().width) * 100);
      return Math.min(100 - least, Math.max(least, percent));
    }

    splitter.addEventListener("pointerdown", function (event) {
      splitter.setPointerCapture(event.pointerId);
      var move = function (moved) {
        var box = panes.getBoundingClientRect();
        setShare(bounded(((moved.clientX - box.left) / box.width) * 100));
      };
      var stop = function () {
        splitter.removeEventListener("pointermove", move);
        splitter.removeEventListener("pointerup", stop);
      };
      splitter.addEventListener("pointermove", move);
      splitter.addEventListener("pointerup", stop);
    });
    splitter.addEventListener("dblclick", function () {
      setShare(composeShare);
    });
    splitter.addEventListener("keydown", function (event) {
      var step = { ArrowLeft: -splitterStep, ArrowRight: splitterStep }[event.key];
      if (!step) {
        return;
      }
      event.preventDefault();
      setShare(bounded(Number(splitter.getAttribute("aria-valuenow")) + step));
    });
  }

  function setUpSideBySide() {
    var editor = document.querySelector("[data-side-editor]");
    if (!editor) {
      return;
    }
    var editors = {};
    var chip = editor.querySelector("[data-side-chip]");
    var changeCount = editor.querySelector("[data-side-changes]");
    var pills = editor.querySelector("[data-check-pills]");
    var problemList = editor.querySelector("[data-check-problems]");
    var panes = editor.querySelector("[data-side-panes]");
    var deploy = editor.querySelector("[data-side-deploy]");
    var status = editor.querySelector("[data-side-status]");
    var actions = editor.querySelectorAll("[data-side-save], [data-side-discard]");
    var saveOnly = editor.querySelector("[data-side-save]:not([data-retest])");
    var toggles = document.querySelectorAll("[data-side-toggle]");
    var untested = editor.hasAttribute("data-untested");
    var checkTimer = 0;
    var checkRound = 0;
    var leaving = false;

    function dirty() {
      return editors.compose.dirty() || editors.env.dirty();
    }

    function pill(name, tone, text) {
      var element = pills.querySelector('[data-pill="' + name + '"]');
      element.className = "chip " + tone;
      element.textContent = text;
    }

    function showStatus() {
      var pending = dirty() || untested;
      chip.className = "chip " + (pending ? "chip-warn" : "chip-" + editor.getAttribute("data-tone"));
      chip.textContent = pending ? "Edited, not tested" : editor.getAttribute("data-chip");
      pills.querySelector('[data-pill="untested"]').hidden = !pending;
    }

    function problemItem(problem, marked) {
      var item = document.createElement("li");
      item.className = "check-problem" + (problem.severity === "warning" ? " is-warning" : "");
      var where = document.createElement("button");
      where.type = "button";
      where.textContent = (problem.file === "env" ? ".env" : "compose.yaml") + (problem.line ? " line " + problem.line : "");
      where.addEventListener("click", function () {
        editors[problem.file].goToLine(problem.line);
      });
      item.appendChild(where);
      item.appendChild(document.createTextNode(problem.message));
      if (problem.severity === "error" && problem.line) {
        marked[problem.file][problem.line] = true;
      }
      if (!problem.review) {
        return item;
      }
      var review = document.createElement("a");
      review.href = editor.getAttribute("data-review");
      review.textContent = "An edit cannot grant it: ask for it on the Review step";
      item.appendChild(review);
      return item;
    }

    function showCheck(result) {
      var marked = { compose: {}, env: {} };
      var items = document.createDocumentFragment();
      var gateClear = result.parses && result.rules_passed === result.rules;
      pill("parses", result.parses ? "chip-ok" : "chip-danger", result.parses ? "YAML parses" : "YAML does not parse");
      pill("gate", gateClear ? "chip-ok" : "chip-danger",
        result.parses ? "Policy gate: " + result.rules_passed + " of " + result.rules + " rules pass" : "Policy gate: waits for YAML that parses");
      pill("defined", result.defined ? "chip-ok" : "chip-danger", result.defined ? "Every ${VAR} is defined in .env" : "A ${VAR} is not defined in .env");
      (result.problems || []).forEach(function (problem) {
        items.appendChild(problemItem(problem, marked));
      });
      problemList.replaceChildren(items);
      editors.compose.markProblems(marked.compose);
      editors.env.markProblems(marked.env);
    }

    function form(retest) {
      var body = new URLSearchParams();
      body.set("compose", editors.compose.value());
      body.set("env", editors.env.value());
      body.set("base", editor.getAttribute("data-base"));
      if (retest) {
        body.set("retest", "1");
      }
      return body;
    }

    function post(address, body) {
      return fetch(address, { method: "POST", headers: { "X-CSRF-Token": editor.getAttribute("data-csrf") }, body: body, credentials: "same-origin" });
    }

    function check() {
      var round = ++checkRound;
      post(editor.getAttribute("data-check"), form(false)).then(function (response) {
        if (!response.ok) {
          throw new Error(response.statusText);
        }
        return response.json();
      }).then(function (result) {
        if (round === checkRound) {
          showCheck(result);
        }
      }, function () {
        if (round === checkRound) {
          pill("parses", "chip-neutral", "Checks unavailable");
        }
      });
    }

    function edited() {
      var count = editors.compose.changes() + editors.env.changes();
      editor.querySelector('[data-dirty="compose"]').hidden = !editors.compose.dirty();
      editor.querySelector('[data-dirty="env"]').hidden = !editors.env.dirty();
      changeCount.hidden = !dirty();
      changeCount.textContent = count === 1 ? "1 change" : count + " changes";
      saveOnly.disabled = !dirty();
      showStatus();
      window.clearTimeout(checkTimer);
      checkTimer = window.setTimeout(check, checkDelay);
    }

    function setBusy(busy, text) {
      actions.forEach(function (button) {
        button.disabled = busy;
      });
      status.textContent = text;
    }

    // save stores the edits as a new version and opens what comes next: the
    // test run, or the Result page at a section the rail asked for.
    function save(retest, section) {
      setBusy(true, "Saving…");
      var submitted = form(retest);
      var savedNumber = 0;
      post(editor.getAttribute("data-save"), submitted).then(function (response) {
        if (response.status === refusedStatus) {
          return response.json().then(function (result) {
            showCheck(result);
            throw new Error("the checks above refuse it");
          });
        }
        if (response.status === staleStatus) {
          return response.json().then(function (answer) {
            throw new Error(answer.message);
          });
        }
        if (!response.ok) {
          throw new Error(response.statusText);
        }
        return response.json();
      }).then(function (answer) {
        if (answer.saved) {
          savedNumber = answer.saved.number;
          editor.setAttribute("data-base", answer.saved.id);
          editors.compose.acceptSaved(submitted.get("compose"));
          editors.env.acceptSaved(submitted.get("env"));
          setBusy(false, "Saved as v" + savedNumber + (answer.validation.requested && !answer.validation.started ? "; the test could not start." : "."));
        }
        leaving = true;
        if (section) {
          store(layoutKey, resultPanel);
        }
        window.location.assign(answer.next + (section ? "#" + section : ""));
      }).catch(function (error) {
        leaving = false;
        setBusy(false, savedNumber ? "Saved as v" + savedNumber + "; open the Files page to continue." : "Save not confirmed: " + error.message);
        saveOnly.disabled = !dirty();
      });
    }

    function setLayout(side) {
      root.classList.toggle(sideBySide, side);
      store(layoutKey, side ? sideBySide : resultPanel);
      toggles.forEach(function (toggle) {
        toggle.setAttribute("aria-expanded", side ? "true" : "false");
      });
      if (!side) {
        return;
      }
      editors.compose.render();
      editors.env.render();
      check();
    }

    function showSection(section) {
      setLayout(false);
      document.getElementById(section).scrollIntoView({ block: "start" });
    }

    editors.compose = codeEditor(editor.querySelector('[data-code-editor="compose"]'), edited);
    editors.env = codeEditor(editor.querySelector('[data-code-editor="env"]'), edited);
    setUpSplitter(panes, editor.querySelector("[data-splitter]"));
    toggles.forEach(function (toggle) {
      toggle.addEventListener("click", function () {
        setLayout(!root.classList.contains(sideBySide));
      });
    });
    document.addEventListener("keydown", function (event) {
      if (event.key !== "\\" || !(event.ctrlKey || event.metaKey)) {
        return;
      }
      event.preventDefault();
      setLayout(!root.classList.contains(sideBySide));
    });
    document.querySelectorAll("[data-rail]").forEach(function (button) {
      button.addEventListener("click", function () {
        var section = button.getAttribute("data-rail");
        var askFirst = button.hasAttribute("data-rail-saves") && dirty();
        if (askFirst && window.confirm("Save your edits as a new version first? Saving, downloading and copying use the latest saved version.")) {
          save(false, section);
          return;
        }
        showSection(section);
      });
    });
    editor.querySelectorAll("[data-side-view]").forEach(function (button) {
      button.addEventListener("click", function () {
        var deployShown = button.getAttribute("data-side-view") === "deploy";
        panes.hidden = deployShown;
        deploy.hidden = !deployShown;
        editor.querySelectorAll("[data-side-view]").forEach(function (other) {
          other.setAttribute("aria-pressed", other === button ? "true" : "false");
        });
      });
    });
    editor.querySelector("[data-provenance-toggle]").addEventListener("change", function (event) {
      editors.compose.showProvenance(event.target.checked);
    });
    editor.querySelector("[data-side-discard]").addEventListener("click", function () {
      if (!dirty() || !window.confirm("Discard your edits to compose.yaml and .env?")) {
        return;
      }
      editors.compose.reset();
      editors.env.reset();
      edited();
    });
    editor.querySelectorAll("[data-side-save]").forEach(function (button) {
      button.addEventListener("click", function () {
        save(button.hasAttribute("data-retest"));
      });
    });
    editor.querySelectorAll("[data-side-reveal]").forEach(function (button) {
      button.addEventListener("click", function (event) {
        if (dirty() && !window.confirm("Revealing secrets reloads the page and drops your unsaved edits. Reveal anyway?")) {
          event.preventDefault();
          return;
        }
        leaving = true;
      });
    });
    window.addEventListener("beforeunload", function (event) {
      if (leaving || !dirty()) {
        return;
      }
      event.preventDefault();
      event.returnValue = "";
    });
    showStatus();
    saveOnly.disabled = true;
    setLayout(root.classList.contains(sideBySide));
  }

  // Away from this machine, plain HTTP carries the password unencrypted;
  // the sign-in pages say so instead of pretending otherwise.
  function warnInsecureContext() {
    var warning = document.querySelector("[data-insecure-context]");
    if (!warning || window.isSecureContext) {
      return;
    }
    warning.hidden = false;
  }

  // Buttons whose action cannot be undone ask first, with the question in
  // data-confirm; without scripts they submit as they are.
  function setUpConfirm() {
    document.querySelectorAll("[data-confirm]").forEach(function (button) {
      button.addEventListener("click", function (event) {
        if (!window.confirm(button.getAttribute("data-confirm"))) {
          event.preventDefault();
        }
      });
    });
  }

  document.addEventListener("DOMContentLoaded", function () {
    setUpToggle();
    warnInsecureContext();
    setUpPasswordReveal();
    setUpSideBySide();
    setUpCopy();
    setUpDropzone();
    setUpTabs();
    setUpComposeEditor();
    setUpEditors();
    setUpShortcuts();
    setUpProjectFilter();
    setUpEnvFilters();
    revealCurrentProject();
    setUpMenus();
    setUpErrorFilter();
    setUpAddRow();
    setUpConnectionForm();
    setUpUnsaved();
    setUpConfirm();
    setUpJob();
    tickElapsed();
    var log = document.querySelector("[data-log]");
    if (log) {
      followLog(log);
    }
  });
})();
