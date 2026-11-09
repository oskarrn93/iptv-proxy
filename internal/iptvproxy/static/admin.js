// createCombobox layers a searchable text-input + filtered dropdown
// over a plain {value, text} list — it owns no backing <select> and
// knows nothing about htmx; the caller's onChoose decides what a pick
// means (see the feed/channel wiring below).
//
// A feed's channel list can run into the tens of thousands of entries
// (some Xtream panels dump that many "live" streams), so this never
// creates one DOM node + listener per option up front — that alone
// freezes the tab for minutes. Instead `options` holds plain {value,
// text} data and render() builds at most MAX_RENDERED matching rows on
// demand (on open, and again per keystroke), with a single delegated
// mousedown listener on the list rather than one per row.
const MAX_RENDERED = 200;

function createCombobox({ search, list, emptyText, onChoose }) {
  const wrapper = search.closest(".combo");
  let options = []; // [{value, text}], source of truth between rebuilds
  let rendered = []; // {value, text} currently in the DOM, index-aligned with list's non-hint children
  let activeIndex = -1;

  function renderedRows() {
    return Array.from(list.children).filter((el) => el.dataset.value !== undefined);
  }

  function setActive(index) {
    const rows = renderedRows();
    rows.forEach((el) => el.classList.remove("active"));
    if (rows.length === 0) {
      activeIndex = -1;
      return;
    }
    activeIndex = ((index % rows.length) + rows.length) % rows.length;
    rows[activeIndex].classList.add("active");
    rows[activeIndex].scrollIntoView({ block: "nearest" });
  }

  function choose(item) {
    search.value = item.text;
    list.hidden = true;
    onChoose(item);
  }

  // Renders at most MAX_RENDERED options whose text matches q
  // (case-insensitive substring) into `list`, replacing whatever was
  // there. This is the only place that creates option DOM nodes.
  function render(q) {
    const query = q.toLowerCase();
    const matches = query ? options.filter((o) => o.text.toLowerCase().includes(query)) : options;
    rendered = matches.slice(0, MAX_RENDERED);

    list.innerHTML = "";
    if (options.length === 0) {
      const empty = document.createElement("div");
      empty.className = "combo-option empty";
      empty.textContent = emptyText;
      list.appendChild(empty);
    } else if (rendered.length === 0) {
      const empty = document.createElement("div");
      empty.className = "combo-option empty";
      empty.textContent = "no matches";
      list.appendChild(empty);
    } else {
      const frag = document.createDocumentFragment();
      rendered.forEach((o) => {
        const item = document.createElement("div");
        item.className = "combo-option";
        item.textContent = o.text;
        item.dataset.value = o.value;
        frag.appendChild(item);
      });
      list.appendChild(frag);
      if (matches.length > rendered.length) {
        const hint = document.createElement("div");
        hint.className = "combo-option empty";
        hint.textContent = `${matches.length - rendered.length} more — keep typing to narrow`;
        list.appendChild(hint);
      }
    }
    setActive(0);
  }

  // Single delegated listener instead of one per row (see MAX_RENDERED
  // comment) — still cheap even when render() is called often.
  list.addEventListener("mousedown", (e) => {
    const el = e.target.closest(".combo-option");
    if (!el || el.dataset.value === undefined) return;
    e.preventDefault();
    const match = rendered.find((o) => o.value === el.dataset.value);
    if (match) choose(match);
  });

  search.addEventListener("input", () => {
    render(search.value);
    list.hidden = false;
  });
  search.addEventListener("focus", () => {
    // Opening the dropdown on an already-chosen field (e.g. the
    // auto-selected first feed) must show every option, not just
    // ones matching the leftover label text — otherwise every other
    // option is filtered out and there's nothing left to click to
    // change the selection. Select the text too, so typing replaces
    // it instead of appending to it.
    render("");
    list.hidden = false;
    search.select();
  });
  search.addEventListener("keydown", (e) => {
    if (list.hidden && e.key !== "Escape") {
      list.hidden = false;
      render(search.value);
    }
    if (e.key === "Escape") {
      list.hidden = true;
      return;
    }
    if (e.key === "ArrowDown") {
      e.preventDefault();
      setActive(activeIndex + 1);
    } else if (e.key === "ArrowUp") {
      e.preventDefault();
      setActive(activeIndex - 1);
    } else if (e.key === "Enter") {
      e.preventDefault();
      if (activeIndex >= 0 && rendered[activeIndex]) choose(rendered[activeIndex]);
    }
  });
  document.addEventListener("click", (e) => {
    if (!wrapper.contains(e.target)) {
      list.hidden = true;
    }
  });

  // setOptions replaces the whole option list (e.g. a fresh feed/
  // channel fetch) and re-renders. autoSelectFirst mirrors a plain
  // default selection, going through choose() so onChoose still runs;
  // with an empty list onChoose(null) runs instead, so the caller can
  // clear whatever depended on the previous selection.
  function setOptions(newOptions, autoSelectFirst) {
    search.value = "";
    activeIndex = -1;
    options = newOptions;
    render("");
    list.hidden = true;
    if (autoSelectFirst && options.length > 0) {
      choose(options[0]);
    } else if (options.length === 0) {
      onChoose(null);
    }
  }

  setOptions([], false);
  return { setOptions };
}

// The channel list has no htmx wiring at all: some Xtream feeds return
// tens of thousands of channels, and htmx's own settle phase (run on
// every node a swap inserts, regardless of how little our own render()
// code does with it — see git history for the measured 100+ second
// freeze this caused) is too expensive at that scale independent of
// any app code. Instead #channel is a plain hidden <input> and
// loadChannelsFor fetches + parses the option list itself via
// DOMParser, which never touches the live document. Declared before
// feedCombo below, since choosing a feed synchronously calls
// loadChannelsFor (even during feedCombo's own construction, for its
// initial empty-options state) and closures can't reference a const
// declared later in the script.
const channelInput = document.getElementById("channel");
const channelSearch = document.getElementById("channel-search");
const channelCombo = createCombobox({
  search: channelSearch,
  list: document.getElementById("channel-list"),
  emptyText: "no channels — pick a feed",
  onChoose: (item) => {
    channelInput.value = item ? item.value : "";
  },
});

// The feed list is small (a handful of configured feeds), so it's
// fine to keep htmx's hx-get/hx-trigger="load" wiring on the outer div
// (see admin.html) — swapping a handful of <option> nodes costs
// nothing. #feed itself stays a real (hidden) <select> purely so its
// own htmx:afterSwap event tells us when a fresh option list has
// landed.
const feedSelect = document.getElementById("feed");
const feedCombo = createCombobox({
  search: document.getElementById("feed-search"),
  list: document.getElementById("feed-list"),
  emptyText: "no feeds configured",
  onChoose: (item) => {
    feedSelect.value = item ? item.value : "";
    loadChannelsFor(item ? item.value : "");
  },
});
feedSelect.addEventListener("htmx:afterSwap", () => {
  const opts = Array.from(feedSelect.options).map((opt) => ({ value: opt.value, text: opt.textContent }));
  feedCombo.setOptions(opts, true);
});

const channelList = document.getElementById("channel-list");

// Shows a spinner + message directly in the dropdown, not just via
// placeholder text — a placeholder is invisible whenever the search
// box already has a value (e.g. re-picking a feed after a channel is
// selected), so it alone isn't an explicit enough loading state for
// fetches that can take a noticeable moment on feeds with tens of
// thousands of channels.
function showChannelListLoading() {
  channelList.innerHTML = "";
  const row = document.createElement("div");
  row.className = "combo-option loading";
  row.innerHTML = '<span class="spinner"></span>loading channels...';
  channelList.appendChild(row);
  channelList.hidden = false;
}

async function loadChannelsFor(feedName) {
  if (!feedName) {
    channelCombo.setOptions([], false);
    channelSearch.placeholder = "Select a feed first...";
    return;
  }
  channelCombo.setOptions([], false);
  channelSearch.placeholder = "Loading channels...";
  channelSearch.disabled = true;
  showChannelListLoading();
  try {
    const res = await fetch("/admin/channels?feed=" + encodeURIComponent(feedName));
    const html = await res.text();
    const doc = new DOMParser().parseFromString(html, "text/html");
    const opts = Array.from(doc.querySelectorAll("option")).map((opt) => ({
      value: opt.value,
      text: opt.textContent,
    }));
    channelSearch.placeholder = opts.length > 0 ? "Search channel..." : "no channels for this feed";
    channelCombo.setOptions(opts, false);
  } finally {
    channelSearch.disabled = false;
  }
}
