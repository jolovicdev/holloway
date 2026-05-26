(function () {
  const table = document.querySelector("[data-webhook-table]");
  const state = document.getElementById("stream-state");

  function setStreamState(text, colorClass) {
    if (!state) {
      return;
    }
    state.innerHTML = '<span class="h-2 w-2 rounded-full ' + colorClass + '"></span>' + escapeHTML(text);
  }

  function escapeHTML(value) {
    return String(value)
      .replace(/&/g, "&amp;")
      .replace(/</g, "&lt;")
      .replace(/>/g, "&gt;")
      .replace(/"/g, "&quot;")
      .replace(/'/g, "&#039;");
  }

  function statusFamily(statusCode) {
    if (statusCode >= 200 && statusCode < 400) {
      return "ok";
    }
    if (statusCode >= 400) {
      return "bad";
    }
    return "";
  }

  function rowHTML(webhook) {
    const family = statusFamily(webhook.status_code);
    const familyAttr = family ? ' data-family="' + family + '"' : "";
    return [
      '<td class="whitespace-nowrap px-4 py-3 text-zinc-600">' + escapeHTML(webhook.received_at) + "</td>",
      '<td class="whitespace-nowrap px-4 py-3 font-semibold">' + escapeHTML(webhook.method) + "</td>",
      '<td class="max-w-[460px] truncate px-4 py-3 font-mono text-xs text-zinc-700">' + escapeHTML(webhook.path) + "</td>",
      '<td class="whitespace-nowrap px-4 py-3"><span class="status-pill" data-status="' + escapeHTML(webhook.status_code) + '"' + familyAttr + ">" + escapeHTML(webhook.status) + "</span></td>",
      '<td class="whitespace-nowrap px-4 py-3 font-mono text-xs text-zinc-500">' + escapeHTML(webhook.short_id) + "</td>"
    ].join("");
  }

  function makeSpan(className, text) {
    const span = document.createElement("span");
    span.className = className;
    span.textContent = text;
    return span;
  }

  function jsonType(value) {
    if (value === null) {
      return "null";
    }
    if (Array.isArray(value)) {
      return "array";
    }
    return typeof value;
  }

  function primitiveNode(value, key) {
    const line = document.createElement("div");
    line.className = "json-line";
    if (key !== null) {
      line.append(makeSpan("json-key", key), makeSpan("json-meta", ": "));
    }

    const type = jsonType(value);
    const rendered = type === "string" ? JSON.stringify(value) : String(value);
    line.append(makeSpan("json-" + type, rendered));
    return line;
  }

  function jsonSummary(key, value) {
    const summary = document.createElement("summary");
    if (key !== null) {
      summary.append(makeSpan("json-key", key), makeSpan("json-meta", ": "));
    }

    if (Array.isArray(value)) {
      summary.append(makeSpan("json-meta", "array[" + value.length + "]"));
      return summary;
    }

    summary.append(makeSpan("json-meta", "object{" + Object.keys(value).length + "}"));
    return summary;
  }

  function jsonNode(value, key, depth) {
    const type = jsonType(value);
    if (type !== "object" && type !== "array") {
      return primitiveNode(value, key);
    }

    const details = document.createElement("details");
    details.className = "json-node";
    details.open = depth < 2;
    details.append(jsonSummary(key, value));

    const entries = Array.isArray(value) ? value.map(function (item, index) {
      return [String(index), item];
    }) : Object.entries(value);
    if (entries.length === 0) {
      details.append(primitiveNode(Array.isArray(value) ? [] : {}, null));
      return details;
    }

    entries.forEach(function (entry) {
      details.append(jsonNode(entry[1], entry[0], depth + 1));
    });
    return details;
  }

  function formatBodyViewers(root) {
    root.querySelectorAll("[data-body-viewer]").forEach(function (viewer) {
      if (viewer.dataset.rendered === "true") {
        return;
      }
      viewer.dataset.rendered = "true";

      const raw = viewer.textContent.trim();
      if (raw === "") {
        viewer.classList.add("body-raw");
        viewer.textContent = "";
        return;
      }

      try {
        const parsed = JSON.parse(raw);
        viewer.classList.add("body-tree");
        viewer.textContent = "";
        viewer.append(jsonNode(parsed, null, 0));
      } catch (_) {
        viewer.classList.add("body-raw");
        viewer.textContent = raw;
      }
    });
  }

  function upsertRow(webhook) {
    if (!table) {
      return;
    }

    const empty = table.querySelector("[data-empty-row]");
    if (empty) {
      empty.remove();
    }

    let row = Array.from(table.querySelectorAll("tr[data-id]")).find(function (candidate) {
      return candidate.dataset.id === webhook.id;
    });
    if (!row) {
      row = document.createElement("tr");
      row.setAttribute("data-id", webhook.id);
      row.setAttribute("hx-get", "/dashboard/webhooks/" + encodeURIComponent(webhook.id));
      row.setAttribute("hx-target", "#inspector");
      row.setAttribute("hx-swap", "innerHTML");
      row.className = "cursor-pointer bg-white hover:bg-emerald-50/60";
      table.prepend(row);
    }

    row.innerHTML = rowHTML(webhook);
    if (window.htmx) {
      window.htmx.process(row);
    }
  }

  document.body.addEventListener("htmx:afterSwap", function (event) {
    if (event.detail.target && event.detail.target.id === "inspector") {
      const current = event.detail.target.querySelector("[data-current-webhook]");
      document.querySelectorAll("[data-webhook-table] tr[data-id]").forEach(function (row) {
        row.dataset.selected = current && row.dataset.id === current.dataset.currentWebhook ? "true" : "false";
      });
      formatBodyViewers(event.detail.target);
    }
  });

  formatBodyViewers(document);

  if (table && table.dataset.liveEnabled === "false") {
    setStreamState("Paused", "bg-zinc-400");
    return;
  }

  if (window.EventSource) {
    const events = new EventSource("/dashboard/events");
    events.addEventListener("open", function () {
      setStreamState("Live", "bg-emerald-600");
    });
    events.addEventListener("error", function () {
      setStreamState("Reconnecting", "bg-amber-500");
    });
    events.addEventListener("webhook", function (event) {
      upsertRow(JSON.parse(event.data));
    });
  } else {
    setStreamState("SSE unavailable", "bg-rose-600");
  }
})();
