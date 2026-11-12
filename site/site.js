// Progressive enhancement only: without this script the page reads the same,
// with the tmux mock showing its first window. Every word shown comes from the
// page itself, so each translation carries its own.
"use strict";

// Copy buttons copy the text of the element their data-copy names.
for (const button of document.querySelectorAll("button[data-copy]")) {
  const label = button.textContent;
  button.addEventListener("click", async () => {
    const text = document.getElementById(button.dataset.copy).textContent;
    try {
      await navigator.clipboard.writeText(text);
      button.textContent = button.dataset.copied || label;
    } catch {
      const range = document.createRange();
      range.selectNodeContents(document.getElementById(button.dataset.copy));
      getSelection().removeAllRanges();
      getSelection().addRange(range);
    }
    setTimeout(() => { button.textContent = label; }, 1600);
  });
}

// The status line flags the section being read, as tmux flags the current
// window with *.
{
  const links = new Map();
  for (const a of document.querySelectorAll(".statusline ol a")) {
    links.set(a.getAttribute("href").slice(1), a);
  }
  const observer = new IntersectionObserver((entries) => {
    for (const entry of entries) {
      if (!entry.isIntersecting) continue;
      for (const a of links.values()) a.removeAttribute("aria-current");
      links.get(entry.target.id)?.setAttribute("aria-current", "true");
    }
  }, { rootMargin: "-40% 0px -55% 0px" });
  for (const id of links.keys()) {
    const section = document.getElementById(id);
    if (section) observer.observe(section);
  }
}

// prefix + A in the mock follows tmag jump's rule: leave the current pane,
// take blocked agents first, then the idle one that has waited longest.
{
  const figure = document.querySelector(".tmux");
  const control = figure?.querySelector(".jump");
  if (figure && control) {
    // since: when each entered its state, in seconds from now (the FOR column
    // of the mock's tmag list, negated), so the smallest has waited longest.
    const agents = [
      { window: 0, state: "blocked", since: -12 },
      { window: 1, state: "working", since: -60 },
      { window: 2, state: "idle", since: -180 },
    ];
    const bell = new Set([0, 2]);
    let current = 1;
    let last = -1;
    const said = control.querySelector("#jump-said");

    const render = () => {
      for (const w of figure.querySelectorAll(".tmux-window")) {
        w.hidden = Number(w.dataset.window) !== current;
      }
      for (const tab of figure.querySelectorAll(".ts-win")) {
        const i = Number(tab.dataset.window);
        const flags = (bell.has(i) ? "!" : "") + (i === current ? "*" : i === last ? "-" : "");
        tab.textContent = tab.textContent.replace(/[!*\- ]*$/, "") + (flags || " ");
      }
    };

    const next = () => agents
      .filter((a) => a.window !== current && (a.state === "blocked" || a.state === "idle"))
      .sort((a, b) => (a.state === "blocked") !== (b.state === "blocked")
        ? (a.state === "blocked" ? -1 : 1)
        : a.since - b.since)[0];

    let visits = 0;
    control.querySelector("#jump").addEventListener("click", () => {
      const target = next();
      if (!target) return;
      last = current;
      current = target.window;
      bell.delete(current);
      visits += 1;
      const key = visits > 2 ? "again" + current : "to" + current;
      said.textContent = control.dataset[key] || control.dataset["to" + current];
      render();
    });
    control.hidden = false;
  }
}
