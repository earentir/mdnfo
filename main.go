package main

import (
	"bytes"
	"errors"
	"fmt"
	"math/rand"
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

// ANSI: SGR sequences and OSC 8 hyperlinks
var ansiRE = regexp.MustCompile(`\x1b$begin:math:display$[0-9;]*[A-Za-z]|\\x1b$end:math:display$8;;.*?\x1b\\|\x1b\\`)

func stripANSI(s string) string { return ansiRE.ReplaceAllString(s, "") }

var (
	reHeading = regexp.MustCompile(`(?m)^\s{0,3}#{1,6}\s+(.*)$`)
	reLink    = regexp.MustCompile(`$begin:math:display$(?P<text>[^$end:math:display$]+)\]$begin:math:text$(?P<dest>[^)]+)$end:math:text$`)
)

// ---------- model ----------

type monoMode int

const (
	monoOff monoMode = iota
	monoGreen
	monoAmber
	monoWhite
)

func (m monoMode) String() string {
	switch m {
	case monoGreen:
		return "Green"
	case monoAmber:
		return "Amber"
	case monoWhite:
		return "Paperwhite"
	default:
		return "Off"
	}
}

type token struct {
	s       string
	isANSI  bool
	byteLen int
}

type model struct {
	filename      string
	rawMarkdown   string
	view          viewport.Model
	renderedFull  string   // glamour output (with ANSI), full document
	renderedLines []string // current (post-processed) lines shown
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

	// CRT/Easy-win toggles
	scanlines bool
	mono      monoMode
	fixed8025 bool
	bbsChrome bool
	degauss   int // remaining frames; when >0, active
	rxBlink   int // frames remaining
	txBlink   int // frames remaining
	rand      *rand.Rand

	// Capability guess
	truecolor  bool
	palette256 bool

	// Modem/baud streaming
	baudrate         int       // e.g., 115200 (bits/sec)
	bytesPerSecond   float64   // derived from baudrate/10 (8N1)
	txStart          time.Time // when stream started
	txBytesAvailable int       // how many bytes should be visible by now
	txLastAvail      int       // prev avail, to blink RX
	streamDone       bool      // once all bytes visible
	streamTokens     []token   // full stream tokenized (ANSI tokens + plain)
	streamTotalBytes int       // total bytes across tokens
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
	// Fixed 80x25 mode keeps a classic canvas
	if m.fixed8025 {
		width = 80
		height = 25
	}
	bodyHeight := height - 2 // header + footer/status
	if bodyHeight < 1 {
		bodyHeight = 1
	}
	wrap := m.wrapWidth
	if wrap <= 0 {
		if m.fixed8025 {
			wrap = 80
		} else {
			wrap = width
		}
	}
	out, err := renderMarkdown(m.rawMarkdown, wrap, m.theme)
	if err != nil {
		m.err = err
		return
	}
	m.renderedFull = out

	// Prepare the transmission tokens for modem emulation
	m.prepareStreamTokens()

	// Build view from current tx progress
	part := m.partialStreamString()
	post := m.applyPostEffects(part)
	m.renderedLines = strings.Split(strings.TrimRight(post, "\n"), "\n")
	m.totalLines = len(m.renderedLines)

	if m.view.Width != width || m.view.Height != bodyHeight {
		m.view.Width = width
		m.view.Height = bodyHeight
	}
	m.view.SetContent(strings.Join(m.renderedLines, "\n"))
	m.buildIndexes()
}

func (m *model) applyPostEffects(s string) string {
	// Optional monochrome filter: strip all color, then recolor lines uniformly
	if m.mono != monoOff {
		plain := stripANSI(s)
		colorOpen, colorClose := monoSGR(m.mono, m.truecolor, m.palette256)
		s = colorOpen + plain + colorClose
	}

	// Scanlines (and degauss jitter)
	if m.scanlines || m.degauss > 0 {
		lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
		for i := range lines {
			if m.degauss > 0 {
				off := 0
				if m.rand.Intn(3) == 0 {
					off = m.rand.Intn(2)
				}
				if off > 0 {
					lines[i] = strings.Repeat(" ", off) + lines[i]
				}
			}
			if i%2 == 1 {
				lines[i] = "\x1b[2m" + lines[i] + "\x1b[22m"
			}
		}
		s = strings.Join(lines, "\n")
	}

	// Brief flash at the start of degauss
	if m.degauss > 0 && m.degauss > degaussTotalFrames()-degaussFlashFrames() {
		s = "\x1b[7m" + s + "\x1b[27m"
	}

	// Clamp to 80 columns visually in 80x25
	if m.fixed8025 {
		s = hardClipColumns(s, 80)
	}
	return s
}

func hardClipColumns(s string, cols int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i := range lines {
		lines[i] = truncateVisibleToWidth(lines[i], cols)
	}
	return strings.Join(lines, "\n")
}

// ---------- streaming / baud emulation ----------

func (m *model) prepareStreamTokens() {
	// Tokenize renderedFull into ANSI and plain segments
	s := m.renderedFull
	m.streamTokens = m.streamTokens[:0]
	m.streamTotalBytes = 0

	idxs := ansiRE.FindAllStringIndex(s, -1)
	last := 0
	for _, span := range idxs {
		// plain before ANSI
		if span[0] > last {
			chunk := s[last:span[0]]
			if chunk != "" {
				bt := len([]byte(chunk))
				m.streamTokens = append(m.streamTokens, token{s: chunk, isANSI: false, byteLen: bt})
				m.streamTotalBytes += bt
			}
		}
		// the ANSI token
		seq := s[span[0]:span[1]]
		bt := len([]byte(seq))
		m.streamTokens = append(m.streamTokens, token{s: seq, isANSI: true, byteLen: bt})
		m.streamTotalBytes += bt
		last = span[1]
	}
	// tail plain
	if last < len(s) {
		chunk := s[last:]
		bt := len([]byte(chunk))
		m.streamTokens = append(m.streamTokens, token{s: chunk, isANSI: false, byteLen: bt})
		m.streamTotalBytes += bt
	}

	// (Re)start stream timing if not already started or if we re-rendered
	if m.txStart.IsZero() {
		m.txStart = time.Now()
	}
	// bytesPerSecond from baudrate with 8N1 overhead ~10 bits/byte
	if m.baudrate > 0 {
		m.bytesPerSecond = float64(m.baudrate) / 10.0
	} else {
		m.bytesPerSecond = 0
	}
	// If baudrate <= 0, show all immediately
	if m.bytesPerSecond <= 0 {
		m.txBytesAvailable = m.streamTotalBytes
		m.streamDone = true
	} else if m.txBytesAvailable > m.streamTotalBytes {
		m.txBytesAvailable = m.streamTotalBytes
		m.streamDone = true
	}
}

func (m *model) partialStreamString() string {
	if m.bytesPerSecond <= 0 {
		return m.renderedFull
	}
	// Calculate allowed bytes based on elapsed time
	elapsed := time.Since(m.txStart).Seconds()
	allowed := int(elapsed * m.bytesPerSecond)
	if allowed > m.streamTotalBytes {
		allowed = m.streamTotalBytes
	}
	if allowed < 0 {
		allowed = 0
	}

	// Blink RX if new bytes arrived
	if allowed > m.txLastAvail {
		m.rxBlink = 6
	}
	m.txLastAvail = allowed
	m.txBytesAvailable = allowed
	m.streamDone = allowed >= m.streamTotalBytes

	if allowed == 0 {
		return ""
	}

	var b strings.Builder
	remain := allowed
	for _, tk := range m.streamTokens {
		if remain <= 0 {
			break
		}
		if tk.byteLen <= remain {
			b.WriteString(tk.s)
			remain -= tk.byteLen
			continue
		}
		// Need to cut inside this token
		if tk.isANSI {
			// Never include partial ANSI; skip it (acts like still buffering).
			break
		}
		// Cut plain text at rune boundaries within byte budget
		wrote := writeRunesWithinBytes(&b, tk.s, remain)
		remain -= wrote
		break
	}
	return b.String()
}

func writeRunesWithinBytes(b *strings.Builder, s string, budget int) int {
	// Append as many runes as fit within 'budget' bytes (UTF-8)
	written := 0
	for _, r := range s {
		n := utf8.RuneLen(r)
		if n < 0 {
			n = 1
		}
		if written+n > budget {
			break
		}
		b.WriteRune(r)
		written += n
	}
	return written
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

func degaussTotalFrames() int { return 30 }
func degaussFlashFrames() int { return 6 }

// ---------- bubbletea plumbing ----------

func initialModel(filename, raw, theme string, wrap int, mod time.Time, size int64, flags startFlags) model {
	v := viewport.New(0, 0)
	v.YPosition = 1

	seed := time.Now().UnixNano()
	truecolor, palette256 := detectColorCaps()

	m := model{
		filename:    filename,
		rawMarkdown: raw,
		view:        v,
		linkIndex:   -1,
		theme:       theme,
		wrapWidth:   wrap,
		fileMod:     mod,
		fileSize:    size,
		scanlines:   flags.scanlines,
		mono:        flags.mono,
		fixed8025:   flags.fixed8025,
		bbsChrome:   flags.bbs,
		rand:        rand.New(rand.NewSource(seed)),
		truecolor:   truecolor,
		palette256:  palette256,
		baudrate:    flags.baudrate,
	}
	return m
}

func (m model) Init() tea.Cmd {
	// Drive ticker for animations and streaming
	return scrollTicker()
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
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
			m.txBlink = 6
			return m, m.startScrollTo(m.view.YOffset - 1)
		case tea.KeyDown:
			m.txBlink = 6
			return m, m.startScrollTo(m.view.YOffset + 1)

		// Smooth page scrolling via animator
		case tea.KeyPgUp, tea.KeyCtrlB:
			m.txBlink = 6
			return m, m.startScrollTo(m.view.YOffset - m.view.Height)
		case tea.KeyPgDown, tea.KeyCtrlF:
			m.txBlink = 6
			return m, m.startScrollTo(m.view.YOffset + m.view.Height)

		case tea.KeyHome:
			m.txBlink = 6
			m.view.GotoTop()
			return m, nil
		case tea.KeyEnd:
			m.txBlink = 6
			m.view.GotoBottom()
			return m, nil

		case tea.KeyTab:
			if len(m.links) > 0 {
				m.txBlink = 6
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
				m.txBlink = 6
				if m.linkIndex == -1 {
					m.linkIndex = len(m.links) - 1
				} else {
					m.linkIndex = (m.linkIndex - 1 + len(m.links)) % len(m.links)
				}
				m.scrollToLink()
			}
			return m, nil
		case tea.KeyEnter:
			m.txBlink = 6
			if m.linkIndex >= 0 && m.linkIndex < len(m.links) {
				m.followLink(m.links[m.linkIndex])
			}
			return m, nil

		default:
			switch strings.ToLower(msg.String()) {
			case "s":
				m.scanlines = !m.scanlines
				m.rxBlink = 6
				m.recalcRendered(m.view.Width, m.view.Height+2)
				return m, nil
			case "m":
				m.mono++
				if m.mono > monoWhite {
					m.mono = monoOff
				}
				m.rxBlink = 6
				m.recalcRendered(m.view.Width, m.view.Height+2)
				return m, nil
			case "b":
				m.bbsChrome = !m.bbsChrome
				m.rxBlink = 6
				m.recalcRendered(m.view.Width, m.view.Height+2)
				return m, nil
			case "d":
				m.degauss = degaussTotalFrames()
				m.rxBlink, m.txBlink = 12, 12
				return m, scrollTicker()
			}
		}

	case scrollTick:
		// Drive animation, blink, degauss, smooth scroll, and streaming progress
		needsRecalc := false

		// Streaming: recompute partial view based on time
		if !m.streamDone && m.bytesPerSecond > 0 {
			_ = m.txBytesAvailable
			// Update allowed bytes and rebuild current content
			part := m.partialStreamString()
			post := m.applyPostEffects(part)
			m.renderedLines = strings.Split(strings.TrimRight(post, "\n"), "\n")
			m.totalLines = len(m.renderedLines)
			m.view.SetContent(strings.Join(m.renderedLines, "\n"))
			needsRecalc = true
		}

		// Smooth scroll animation
		if m.animating {
			cur := m.view.YOffset
			tgt := m.targetOffset
			if cur != tgt {
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
				if newOff == tgt {
					m.animating = false
				}
				needsRecalc = true
			} else {
				m.animating = false
			}
		}

		if m.degauss > 0 {
			m.degauss--
			// Re-apply post effects for jitter/flash while active
			part := m.partialStreamString()
			post := m.applyPostEffects(part)
			m.renderedLines = strings.Split(strings.TrimRight(post, "\n"), "\n")
			m.view.SetContent(strings.Join(m.renderedLines, "\n"))
			needsRecalc = true
		}
		if m.rxBlink > 0 {
			m.rxBlink--
			needsRecalc = true
		}
		if m.txBlink > 0 {
			m.txBlink--
			needsRecalc = true
		}
		if needsRecalc || m.scanlines || m.bbsChrome || m.degauss > 0 || m.animating {
			return m, scrollTicker()
		}
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

// ---------- indexing (restored) ----------

func (m *model) buildIndexes() {
	// Build indexes from the CURRENT visible content (post-effects stripped),
	// so anchors/links scroll to what the user actually sees right now.
	plain := stripANSI(strings.Join(m.renderedLines, "\n"))

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

// ---------- view ----------

func (m model) View() string {
	if m.err != nil {
		return fmt.Sprintf("error: %v\n", m.err)
	}
	w := m.view.Width
	if w <= 0 {
		w = 80
	}

	// Right side: file mod time (ISO 8601) + human size + caps
	caps := "TC"
	if !m.truecolor && m.palette256 {
		caps = "256"
	}
	if !m.truecolor && !m.palette256 {
		caps = "16"
	}

	right := fmt.Sprintf("%s %s [%s]", m.fileMod.Format(time.RFC3339), humanSize(m.fileSize), caps)

	left := m.filename
	available := w - displayWidth(right) - 1
	if available < 1 {
		available = 1
	}
	left = truncateToWidth(left, available)

	// Mode indicators
	badges := []string{}
	if m.fixed8025 {
		badges = append(badges, "80x25")
	}
	if m.scanlines {
		badges = append(badges, "Scanlines")
	}
	if m.mono != monoOff {
		badges = append(badges, "Mono:"+m.mono.String())
	}
	if m.bbsChrome {
		badges = append(badges, "BBS")
	}
	if m.baudrate > 0 && !m.streamDone {
		badges = append(badges, fmt.Sprintf("RX %.0fB/s", m.bytesPerSecond))
	}
	if len(badges) > 0 {
		left = left + "  [" + strings.Join(badges, " | ") + "]"
	}

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

	footer := progress
	if m.bbsChrome {
		footer = m.bbsStatusLine(w)
	}

	return header + "\n" + m.view.View() + "\n" + footer
}

func (m model) bbsStatusLine(w int) string {
	// e.g., " CONNECT 115200  RX:· TX:·  [s]canlines [m]ono [b]bs [d]egauss  [q]uit "
	rx := "·"
	tx := "·"
	if m.rxBlink > 0 {
		rx = "●"
	}
	if m.txBlink > 0 {
		tx = "●"
	}
	label := fmt.Sprintf(" CONNECT %d  RX:%s TX:%s  [s]canlines [m]ono [b]bs [d]egauss  [q]uit ", m.baudrate, rx, tx)
	if displayWidth(label) >= w {
		return truncateToWidth(label, w)
	}
	pad := strings.Repeat(" ", w-displayWidth(label))
	return label + pad
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

// truncateVisibleToWidth truncates by visible width (ANSI-safe) for simple UI strings we control.
func truncateVisibleToWidth(s string, w int) string {
	plain := stripANSI(s)
	if displayWidth(plain) <= w {
		return s
	}
	runes := []rune(plain)
	return string(runes[:w])
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
	return fmt.Sprintf("%.2f%s", f, u[i])
}

// monochrome color sequences (prefer truecolor; fall back to 256/16-color)
func monoSGR(m monoMode, truecolor, palette256 bool) (open, close string) {
	var fg string
	switch m {
	case monoGreen:
		fg = "32"
	case monoAmber:
		fg = "33"
	case monoWhite:
		fg = "37"
	default:
		return "", ""
	}
	if palette256 {
		switch m {
		case monoGreen:
			fg = "38;5;82"
		case monoAmber:
			fg = "38;5;214"
		case monoWhite:
			fg = "38;5;252"
		}
	}
	if truecolor {
		switch m {
		case monoGreen:
			fg = "38;2;0;255;128"
		case monoAmber:
			fg = "38;2;255;176;0"
		case monoWhite:
			fg = "38;2;230;230;230"
		}
	}
	return "\x1b[" + fg + "m", "\x1b[0m"
}

// crude capability detection (best-effort)
func detectColorCaps() (truecolor bool, palette256 bool) {
	tc := os.Getenv("COLORTERM")
	if strings.Contains(strings.ToLower(tc), "truecolor") || strings.Contains(strings.ToLower(tc), "24bit") {
		truecolor = true
	}
	termVar := strings.ToLower(os.Getenv("TERM"))
	if strings.Contains(termVar, "256color") || strings.Contains(termVar, "xterm") || strings.Contains(termVar, "screen-256color") {
		palette256 = true
	}
	if truecolor {
		palette256 = true
	}
	return
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

// ---------- flags ----------

type startFlags struct {
	style     string
	wrap      int
	scanlines bool
	mono      monoMode
	fixed8025 bool
	bbs       bool
	baudrate  int
}

// ---------- cobra CLI ----------

func main() {
	var flags startFlags
	flags.style = "auto"
	flags.baudrate = 115200

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
			m := initialModel(abs, string(b), flags.style, flags.wrap, fi.ModTime(), fi.Size(), flags)

			// size to the real terminal BEFORE starting Bubble Tea
			w, h := 80, 24
			if ww, hh, err := term.GetSize(int(os.Stdout.Fd())); err == nil && ww > 0 && hh > 0 {
				w, h = ww, hh
			}

			// first render and start streaming clock
			m.txStart = time.Now()
			m.recalcRendered(w, h)

			prog := tea.NewProgram(m, tea.WithAltScreen())
			_, err = prog.Run()
			return err
		},
	}

	cmd.Flags().StringVar(&flags.style, "style", "auto", "glamour style: auto, dark, light, notty, dracula, pink, or a JSON style file path")
	cmd.Flags().IntVar(&flags.wrap, "wrap", 0, "wrap width (0 = auto to terminal width)")
	cmd.Flags().BoolVar(&flags.scanlines, "scanlines", false, "enable CRT-like scanlines")
	cmd.Flags().BoolVar(&flags.bbs, "bbs", false, "enable BBS-style status line")
	cmd.Flags().BoolVar(&flags.fixed8025, "80x25", false, "force classic 80x25 canvas")
	cmd.Flags().IntVar(&flags.baudrate, "baudrate", 9600, "modem baud rate (bits/sec), e.g., 1200, 9600, 115200, 256000")
	var monoStr string
	cmd.Flags().StringVar(&monoStr, "mono", "off", "monochrome CRT mode: off, green, amber, white")

	cmd.PreRunE = func(cmd *cobra.Command, args []string) error {
		switch strings.ToLower(strings.TrimSpace(monoStr)) {
		case "off", "":
			flags.mono = monoOff
		case "green":
			flags.mono = monoGreen
		case "amber":
			flags.mono = monoAmber
		case "white", "paperwhite":
			flags.mono = monoWhite
		default:
			return fmt.Errorf("invalid --mono value: %q (use off|green|amber|white)", monoStr)
		}
		if flags.baudrate < 0 {
			return fmt.Errorf("invalid --baudrate: %d", flags.baudrate)
		}
		return nil
	}

	if err := cmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
