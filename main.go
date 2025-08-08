package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/glamour"
	"github.com/mattn/go-isatty"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// ---------- link & heading helpers ----------

type link struct {
	text         string
	target       string // url or #anchor
	renderedLine int
}

type heading struct {
	text         string
	anchor       string // github-style slug
	renderedLine int
}

func slugify(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == ' ' || r == '-' {
			b.WriteRune(r)
		}
	}
	return strings.Trim(strings.Join(strings.Fields(strings.ReplaceAll(b.String(), " ", "-")), "-"), "-")
}

var ansiRE = regexp.MustCompile(`\x1b\[[0-9;]*[A-Za-z]|\x1b\]8;;.*?\x1b\\|\x1b\\`)

func stripANSI(s string) string { return ansiRE.ReplaceAllString(s, "") }

var (
	reHeading = regexp.MustCompile(`(?m)^\s{0,3}#{1,6}\s+(.*)$`)
	reLink    = regexp.MustCompile(`\[(?P<text>[^\]]+)\]\((?P<dest>[^)]+)\)`)
)

// ---------- model ----------

type model struct {
	filename      string
	rawMarkdown   string
	view          viewport.Model
	rendered      string
	renderedLines []string
	totalLines    int

	links     []link
	headings  []heading
	linkIndex int // -1 none

	theme     string
	wrapWidth int
	err       error

	// file metadata (for header)
	fileMod  time.Time
	fileSize int64

	// smooth scroll animation (works for single-line and page)
	animating    bool
	targetOffset int
}

// ---------- rendering ----------

func renderMarkdown(raw string, width int, style string) (string, error) {
	opts := []glamour.TermRendererOption{
		glamour.WithWordWrap(width),
	}

	switch strings.ToLower(strings.TrimSpace(style)) {
	case "", "auto":
		opts = append(opts, glamour.WithAutoStyle())
	case "dark", "light", "notty", "dracula", "pink":
		opts = append(opts, glamour.WithStylePath(style))
	default:
		// If it's a file path to a JSON style, use it; else fall back to auto.
		if _, err := os.Stat(style); err == nil {
			opts = append(opts, glamour.WithStylesFromJSONFile(style))
		} else {
			opts = append(opts, glamour.WithAutoStyle())
		}
	}

	r, err := glamour.NewTermRenderer(opts...)
	if err != nil {
		return "", err
	}
	return r.Render(raw)
}

func (m *model) recalcRendered(width, height int) {
	bodyHeight := height - 2 // header + footer
	if bodyHeight < 1 {
		bodyHeight = 1
	}
	wrap := m.wrapWidth
	if wrap <= 0 {
		wrap = width
	}
	out, err := renderMarkdown(m.rawMarkdown, wrap, m.theme)
	if err != nil {
		m.err = err
		return
	}
	m.rendered = out
	m.renderedLines = strings.Split(strings.TrimRight(out, "\n"), "\n")
	m.totalLines = len(m.renderedLines)

	if m.view.Width != width || m.view.Height != bodyHeight {
		m.view.Width = width
		m.view.Height = bodyHeight
	}
	m.view.SetContent(m.rendered)
	m.buildIndexes()
}

func (m *model) buildIndexes() {
	plain := stripANSI(m.rendered)

	m.headings = nil
	for _, mm := range reHeading.FindAllStringSubmatch(m.rawMarkdown, -1) {
		txt := strings.TrimSpace(mm[1])
		if txt == "" {
			continue
		}
		anc := slugify(txt)
		idx := indexLineOf(plain, txt)
		m.headings = append(m.headings, heading{text: txt, anchor: anc, renderedLine: idx})
	}

	m.links = nil
	for _, mm := range reLink.FindAllStringSubmatchIndex(m.rawMarkdown, -1) {
		text := m.rawMarkdown[mm[2]:mm[3]]
		dest := m.rawMarkdown[mm[4]:mm[5]]
		needle := dest
		if strings.HasPrefix(dest, "#") {
			needle = text
		}
		idx := indexLineOf(plain, needle)
		m.links = append(m.links, link{text: text, target: dest, renderedLine: idx})
	}
	if len(m.links) == 0 {
		m.linkIndex = -1
	} else if m.linkIndex >= len(m.links) {
		m.linkIndex = len(m.links) - 1
	}
}

func indexLineOf(haystack, needle string) int {
	if needle == "" {
		return -1
	}
	pos := strings.Index(haystack, needle)
	if pos < 0 {
		return -1
	}
	return bytes.Count([]byte(haystack[:pos]), []byte("\n"))
}

// ---------- animation helpers ----------

type scrollTick struct{}

func scrollTicker() tea.Cmd {
	// ~60 FPS; smooth without cooking the CPU
	return tea.Tick(time.Second/60, func(time.Time) tea.Msg { return scrollTick{} })
}

func (m *model) startScrollTo(target int) tea.Cmd {
	maxOffset := max(0, m.totalLines-m.view.Height)
	if target < 0 {
		target = 0
	}
	if target > maxOffset {
		target = maxOffset
	}
	m.targetOffset = target
	if m.view.YOffset == m.targetOffset {
		m.animating = false
		return nil
	}
	m.animating = true
	return scrollTicker()
}

// ---------- bubbletea plumbing ----------

func initialModel(filename, raw, theme string, wrap int, mod time.Time, size int64) model {
	v := viewport.New(0, 0)
	v.YPosition = 1

	m := model{
		filename:    filename,
		rawMarkdown: raw,
		view:        v,
		linkIndex:   -1,
		theme:       theme,
		wrapWidth:   wrap,
		fileMod:     mod,
		fileSize:    size,
	}
	return m
}

func (m model) Init() tea.Cmd {
	// No periodic tick needed; header uses file modtime, not now.
	return nil
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		// Re-render AND forward the message to the viewport so it updates its internals.
		m.recalcRendered(msg.Width, msg.Height)
		var cmd tea.Cmd
		m.view, cmd = m.view.Update(msg)
		return m, cmd

	case tea.KeyMsg:
		// quit on q or Q
		if msg.String() == "q" || msg.String() == "Q" {
			return m, tea.Quit
		}
		switch msg.Type {
		case tea.KeyEsc:
			return m, tea.Quit

		// Smooth single-line scrolling via animator
		case tea.KeyUp:
			return m, m.startScrollTo(m.view.YOffset - 1)
		case tea.KeyDown:
			return m, m.startScrollTo(m.view.YOffset + 1)

		// Smooth page scrolling via animator
		case tea.KeyPgUp, tea.KeyCtrlB:
			return m, m.startScrollTo(m.view.YOffset - m.view.Height)
		case tea.KeyPgDown, tea.KeyCtrlF:
			return m, m.startScrollTo(m.view.YOffset + m.view.Height)

		case tea.KeyHome:
			m.view.GotoTop()
			return m, nil
		case tea.KeyEnd:
			m.view.GotoBottom()
			return m, nil
		case tea.KeyTab:
			if len(m.links) > 0 {
				if m.linkIndex == -1 {
					m.linkIndex = 0
				} else {
					m.linkIndex = (m.linkIndex + 1) % len(m.links)
				}
				m.scrollToLink()
			}
			return m, nil
		case tea.KeyShiftTab:
			if len(m.links) > 0 {
				if m.linkIndex == -1 {
					m.linkIndex = len(m.links) - 1
				} else {
					m.linkIndex = (m.linkIndex - 1 + len(m.links)) % len(m.links)
				}
				m.scrollToLink()
			}
			return m, nil
		case tea.KeyEnter:
			if m.linkIndex >= 0 && m.linkIndex < len(m.links) {
				m.followLink(m.links[m.linkIndex])
			}
			return m, nil
		}
		return m, nil

	case scrollTick:
		if !m.animating {
			return m, nil
		}
		cur := m.view.YOffset
		tgt := m.targetOffset
		if cur == tgt {
			m.animating = false
			return m, nil
		}
		// Ease-out: big steps far away, 1-line steps near target.
		diff := tgt - cur
		step := diff / 5
		if step == 0 {
			if diff > 0 {
				step = 1
			} else {
				step = -1
			}
		}
		newOff := cur + step
		if (diff > 0 && newOff > tgt) || (diff < 0 && newOff < tgt) {
			newOff = tgt
		}
		m.view.SetYOffset(newOff)
		if newOff != tgt {
			return m, scrollTicker()
		}
		m.animating = false
		return m, nil
	}

	var cmd tea.Cmd
	m.view, cmd = m.view.Update(msg)
	return m, cmd
}

func (m *model) scrollToLink() {
	if m.linkIndex < 0 || m.linkIndex >= len(m.links) {
		return
	}
	line := m.links[m.linkIndex].renderedLine
	if line < 0 {
		return
	}
	target := line - m.view.Height/2
	if target < 0 {
		target = 0
	}
	if target > m.totalLines-m.view.Height {
		target = m.totalLines - m.view.Height
	}
	if target < 0 {
		target = 0
	}
	m.view.SetYOffset(target)
}

func (m *model) followLink(l link) {
	dest := strings.TrimSpace(l.target)
	if dest == "" {
		return
	}
	if strings.HasPrefix(dest, "#") {
		anc := strings.TrimPrefix(dest, "#")
		for _, h := range m.headings {
			if h.anchor == anc || slugify(h.text) == anc || slugify(anc) == h.anchor {
				if h.renderedLine >= 0 {
					m.view.SetYOffset(clamp(h.renderedLine, 0, max(0, m.totalLines-m.view.Height)))
					return
				}
			}
		}
		if l.renderedLine >= 0 {
			m.view.SetYOffset(clamp(l.renderedLine, 0, max(0, m.totalLines-m.view.Height)))
		}
		return
	}
	_ = openURL(dest)
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// ---------- view ----------

func (m model) View() string {
	if m.err != nil {
		return fmt.Sprintf("error: %v\n", m.err)
	}
	w := m.view.Width
	if w <= 0 {
		w = 80
	}

	// Right side: file mod time (ISO 8601) + human size
	right := fmt.Sprintf("%s %s", m.fileMod.Format(time.RFC3339), humanSize(m.fileSize))

	left := m.filename
	available := w - displayWidth(right) - 1
	if available < 1 {
		available = 1
	}
	left = truncateToWidth(left, available)
	header := fmt.Sprintf("%-*s %s", available, left, right)

	// current line = last visible line, capped at total
	current := m.view.YOffset + m.view.Height
	if current > m.totalLines {
		current = m.totalLines
	}
	if current < 1 && m.totalLines > 0 {
		current = 1
	}
	total := max(1, m.totalLines)

	// progress ratio based on scroll offset (start 0, end 1 at bottom)
	ratio := 0.0
	den := float64(max(1, m.totalLines-m.view.Height))
	if den > 0 {
		ratio = float64(m.view.YOffset) / den
		if ratio < 0 {
			ratio = 0
		}
		if ratio > 1 {
			ratio = 1
		}
	}
	progress := drawProgressBar(w, ratio, fmt.Sprintf(" %d / %d ", current, total))

	return header + "\n" + m.view.View() + "\n" + progress
}

func drawProgressBar(width int, ratio float64, label string) string {
	if width < 3 {
		return strings.Repeat("█", width)
	}
	fill := int(float64(width) * ratio)
	if fill < 0 {
		fill = 0
	}
	if fill > width {
		fill = width
	}
	var b strings.Builder
	b.Grow(width)
	b.WriteString(strings.Repeat("█", fill))
	if fill < width {
		b.WriteString(strings.Repeat("░", width-fill))
	}
	bar := b.String()

	if len(label) > 0 && len(label) < width {
		start := (width - len(label)) / 2
		runes := []rune(bar)
		labelRunes := []rune(label)
		for i := 0; i < len(labelRunes) && start+i < len(runes); i++ {
			runes[start+i] = labelRunes[i]
		}
		bar = string(runes)
	}
	return bar
}

func displayWidth(s string) int {
	return utf8.RuneCountInString(s)
}

func truncateToWidth(s string, w int) string {
	if displayWidth(s) <= w {
		return s
	}
	runes := []rune(s)
	return string(runes[:w])
}

// ---------- util ----------

func humanSize(n int64) string {
	u := []string{"B", "KB", "MB", "GB", "TB", "PB"}
	if n < 1024 {
		return fmt.Sprintf("%dB", n)
	}
	f := float64(n)
	i := 0
	for f >= 1024 && i < len(u)-1 {
		f /= 1024
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%dB", n)
	}
	// 2 decimal places like 12.34KB/MB/GB...
	return fmt.Sprintf("%.2f%s", f, u[i])
}

// ---------- openURL ----------

func openURL(u string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", u)
	case "windows":
		cmd = exec.Command("cmd", "/c", "start", u)
	default:
		cmd = exec.Command("xdg-open", u)
	}
	cmd.Stdout, cmd.Stderr, cmd.Stdin = nil, nil, nil
	return cmd.Start()
}

// ---------- cobra CLI ----------

func main() {
	var style string
	var wrap int

	cmd := &cobra.Command{
		Use:   "mdnfo <file.md>",
		Short: "Old-school NFO-style Markdown viewer (terminal-only)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			path := args[0]
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if !isatty.IsTerminal(os.Stdout.Fd()) && !isatty.IsCygwinTerminal(os.Stdout.Fd()) {
				return errors.New("stdout is not a TTY (refusing to render ANSI output)")
			}
			abs, _ := filepath.Abs(path)

			// file metadata
			fi, err := os.Stat(path)
			if err != nil {
				return err
			}

			// create model
			m := initialModel(abs, string(b), style, wrap, fi.ModTime(), fi.Size())

			// size to the real terminal BEFORE starting Bubble Tea
			if w, h, err := term.GetSize(int(os.Stdout.Fd())); err == nil && w > 0 && h > 0 {
				m.recalcRendered(w, h)
			} else {
				// fallback
				m.recalcRendered(80, 24)
			}

			prog := tea.NewProgram(m, tea.WithAltScreen())
			_, err = prog.Run()
			return err
		},
	}

	cmd.Flags().StringVar(&style, "style", "auto", "glamour style: auto, dark, light, notty, dracula, pink, or a JSON style file path")
	cmd.Flags().IntVar(&wrap, "wrap", 0, "wrap width (0 = auto to terminal width)")

	if err := cmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
