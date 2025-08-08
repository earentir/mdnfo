# mdnfo — Old-school NFO-style Markdown viewer (terminal only)

`mdnfo` is a fast, keyboard-driven Markdown viewer for the terminal. It renders rich Markdown with colors, gives you smooth one-line and full-page scrolling, shows a retro progress bar, and supports both internal (`#anchor`) and external links.

Built with [Bubble Tea](https://github.com/charmbracelet/bubbletea), [Glamour](https://github.com/charmbracelet/glamour), and [Cobra](https://github.com/spf13/cobra).

---

## Features

* **Full-color Markdown** rendering in your TTY using Glamour.
* **Smooth scrolling:** arrow keys for single lines; PageUp/PageDown for whole pages.
* **Jump keys:** Home/End to go to the first/last line.
* **Links:**

  * Internal `#anchor` links jump to headings in the same file.
  * External links open in your system browser.
  * Tab / Shift+Tab cycles through links; Enter follows the selected link.
* **Top status line:** full file path (left) + live ISO-8601 time (right).
* **Bottom progress bar:** full-width bar with “current line / total lines”.
* **100% terminal:** no GUI, no server, no dependencies beyond Go modules.

---

## Installation

### Prerequisites

* Go **1.20+**
* A true terminal (TTY). The viewer refuses to render when stdout is not a TTY.

### Build from source

```bash
git clone <your-repo-url> mdnfo
cd mdnfo
go mod tidy
go build
```

Run:

```bash
./mdnfo README.md
```

Or directly:

```bash
go run . /path/to/file.md
```

---

## Usage

```
mdnfo <file.md> [flags]
```

### Common examples

```bash
# Auto style, wrap to terminal width
mdnfo docs/guide.md

# Force light style and hard wrap at 100 columns
mdnfo --style light --wrap 100 docs/guide.md

# Use a custom Glamour style from file
mdnfo --style .config/glamour-dracula.json notes.md
```

### Flags

| Flag      | Type   | Default | Description                                                                                         |
| --------- | ------ | ------- | --------------------------------------------------------------------------------------------------- |
| `--style` | string | `auto`  | Glamour style: `auto`, `dark`, `light`, `notty`, `dracula`, `pink`, or a path to a JSON style file. |
| `--wrap`  | int    | `0`     | Hard wrap width. `0` = auto (match terminal width).                                                 |

---

## Keybindings

| Key               | Action                      |
| ----------------- | --------------------------- |
| ↑ / ↓             | Scroll up / down **1 line** |
| PageUp / Ctrl-B   | Scroll up **one page**      |
| PageDown / Ctrl-F | Scroll down **one page**    |
| Home              | Jump to **first line**      |
| End               | Jump to **last line**       |
| Tab / Shift+Tab   | Select next / previous link |
| Enter             | Follow selected link        |
| Esc               | Exit viewer                 |

---

## Link support

* **Internal links**: `[Intro](#introduction)` moves the viewport to the matching `# Introduction` heading.
* **External links**: `[Website](https://example.org)` opens in your default browser via `open` (macOS), `xdg-open` (Linux), or `start` (Windows).
* Links are detected from standard inline Markdown syntax (`[text](dest)`).

> Note: Heading and link positions are computed against the **rendered** output, so in extremely stylized themes the jump target is an approximation, but practically it lands right on the heading or very close.

---

## UI details

* **Header**: `/<full/path/to/file.md>                                          2025-08-06T12:34:56Z`
* **Footer**: a full-width progress bar using block characters with a centered label like `120 / 980`.
* **Wrapping**: By default, lines are wrapped to your terminal width; override with `--wrap`.

---

## Troubleshooting

* **“stdout is not a TTY (refusing to render ANSI output)”**
  Run `mdnfo` directly in a terminal (don’t pipe/redirect its output).
* **Colors don’t look right**
  Try a different `--style` (e.g. `dark`, `light`) or supply your own Glamour style JSON.
* **Links don’t open**
  Ensure `xdg-open` (Linux) or `open` (macOS) is available in `PATH`. On Windows, `start` is used via `cmd`.

---

## Roadmap / Ideas

* Optional OSC-8 hyperlink emission for clickable links in capable terminals.
* Search (`/`), incremental find, and jump to next/previous heading.
* File reload (`r`) / auto-reload on change.
* Optional line numbers and a mini-map.

---

## Development

```bash
# Lint/format (optional; use your preferred tools)
go fmt ./...
go vet ./...

# Run during development
go run . ./testdata/sample.md
```

Project entry point: `main.go`. Major packages used: Bubble Tea (UI loop), Glamour (Markdown renderer), Cobra (CLI).

---

## License

MIT © You — feel free to swap in your preferred license.

---

## Credits

* Charmbracelet projects for Bubble Tea & Glamour.
* Cobra for CLI ergonomics.
