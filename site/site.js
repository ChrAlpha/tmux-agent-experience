// Progressive enhancement only: without this script the page reads the same,
// with the tmux mock showing its first window. Every word shown comes from the
// page itself, so each translation carries its own.
"use strict";

// jumpNext is the agent tmag jump takes next: blocked agents first, then
// finished ones, oldest first, leaving the current pane. A settled agent is
// no target.
function jumpNext(agents, current) {
  return agents
    .filter((a) => a.window !== current && (a.state === "blocked" || a.state === "finished"))
    .sort((a, b) => (a.state === "blocked") !== (b.state === "blocked")
      ? (a.state === "blocked" ? -1 : 1)
      : a.since - b.since)[0];
}

// jumpVisit takes the next agent, settles a visited finished one, and returns
// it; null when no agent is waiting.
function jumpVisit(agents, current) {
  const target = jumpNext(agents, current);
  if (!target) return null;
  if (target.state === "finished") target.state = "idle";
  return target;
}

// jumpCounts counts agents as the status line does.
function jumpCounts(agents) {
  const n = { blocked: 0, finished: 0, working: 0, idle: 0 };
  for (const a of agents) n[a.state] += 1;
  return n;
}

if (typeof module !== "undefined" && module.exports) {
  module.exports = { jumpNext, jumpVisit, jumpCounts };
}

if (typeof document !== "undefined") {
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
  // take blocked agents first, then the finished one that has waited longest.
  // Visiting a finished agent settles it, so it is no longer a target and the
  // status line counts it as idle.
  {
    const figure = document.querySelector(".tmux");
    const control = figure?.querySelector(".jump");
    const status = figure?.querySelector(".ts-counts");
    if (figure && control) {
      // since: when each entered its state, in seconds from now (the FOR column
      // of the mock's tmag list, negated), so the smallest has waited longest.
      const agents = [
        { window: 0, state: "blocked", since: -12 },
        { window: 1, state: "working", since: -60 },
        { window: 2, state: "finished", since: -180 },
      ];
      const bell = new Set([0, 2]);
      let current = 1;
      let last = -1;
      const said = control.querySelector("#jump-said");

      const renderStatus = () => {
        if (!status) return;
        const n = jumpCounts(agents);
        const parts = [];
        if (n.blocked) parts.push(`<b class="ts-blocked">${n.blocked} blocked</b>`);
        if (n.finished) parts.push(`${n.finished} finished`);
        if (n.working) parts.push(`${n.working} working`);
        if (n.idle) parts.push(`${n.idle} idle`);
        status.innerHTML = parts.join(" · ");
      };

      const render = () => {
        for (const w of figure.querySelectorAll(".tmux-window")) {
          w.hidden = Number(w.dataset.window) !== current;
        }
        for (const tab of figure.querySelectorAll(".ts-win")) {
          const i = Number(tab.dataset.window);
          const flags = (bell.has(i) ? "!" : "") + (i === current ? "*" : i === last ? "-" : "");
          tab.textContent = tab.textContent.replace(/[!*\- ]*$/, "") + (flags || " ");
        }
        renderStatus();
      };

      let visits = 0;
      control.querySelector("#jump").addEventListener("click", () => {
        const target = jumpVisit(agents, current);
        if (!target) {
          said.textContent = control.dataset.none || said.textContent;
          return;
        }
        last = current;
        current = target.window;
        bell.delete(current);
        visits += 1;
        const key = visits > 2 ? "again" + current : "to" + current;
        said.textContent = control.dataset[key] || control.dataset["to" + current];
        render();
      });
      render();
      control.hidden = false;
    }
  }
}
