// Progressive enhancement for the reaction picker: search, category tabs and a
// live list of selected emoji. Without JavaScript the grid of checkboxes works.
document.addEventListener("DOMContentLoaded", () => {
  for (const picker of document.querySelectorAll("[data-picker]")) {
    const search = picker.querySelector("[data-picker-search]");
    const tabs = picker.querySelector("[data-picker-tabs]");
    const selected = picker.querySelector("[data-picker-selected]");
    const items = [...picker.querySelectorAll("label.emoji")];
    search.hidden = false;
    tabs.hidden = false;
    const render = () => {
      selected.textContent = "";
      for (const item of items) {
        const box = item.querySelector("input");
        if (!box.checked) continue;
        const chip = document.createElement("span");
        chip.className = "chip";
        chip.textContent = (item.querySelector(".glyph").textContent || "") + " :" + box.value + ":";
        selected.append(chip, " ");
      }
      if (!selected.textContent) selected.textContent = "none yet";
    };
    search.addEventListener("input", () => {
      const q = search.value.trim().toLowerCase().replaceAll(":", "");
      for (const item of items) item.hidden = q !== "" && !item.dataset.keywords.includes(q);
      for (const section of picker.querySelectorAll("[data-group-section]")) {
        section.hidden = ![...section.querySelectorAll("label.emoji")].some((i) => !i.hidden);
      }
    });
    search.addEventListener("keydown", (e) => {
      if (e.key !== "Enter") return;
      e.preventDefault();
      const first = items.find((i) => !i.hidden);
      if (first) { first.querySelector("input").checked = true; render(); }
    });
    for (const tab of tabs.querySelectorAll("[data-group]")) {
      tab.addEventListener("click", () => {
        const section = [...picker.querySelectorAll("[data-group-section]")].find((s) => s.dataset.groupSection === tab.dataset.group);
        if (section) section.scrollIntoView({ block: "nearest" });
      });
    }
    picker.addEventListener("change", render);
    render();
  }
});

// Copy buttons: data-copy names the element whose text is copied. Falls back to
// selecting the text where the Clipboard API is unavailable (plain-HTTP servers).
document.addEventListener("click", async (event) => {
  const button = event.target.closest("[data-copy]");
  if (!button) return;
  const source = document.getElementById(button.dataset.copy);
  if (!source) return;
  const label = button.textContent;
  try {
    await navigator.clipboard.writeText(source.textContent);
    button.textContent = "Copied ✓";
  } catch {
    const range = document.createRange();
    range.selectNodeContents(source);
    const selection = window.getSelection();
    selection.removeAllRanges();
    selection.addRange(range);
    button.textContent = "Selected · press Ctrl/⌘+C";
  }
  setTimeout(() => { button.textContent = label; }, 2000);
});
