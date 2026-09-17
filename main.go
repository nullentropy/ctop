// ctop is a system monitor built with caution
//
//	go run .                 # desktop window (default)
//	go run . -serve          # serve browsers on :8080 instead
//	go tool appbundle -pkg . -name ctop   # bundled .app
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	caution "github.com/nullentropy/caution/go"
	"github.com/nullentropy/caution/go/native"
	"github.com/nullentropy/ctop/sys"
)

const (
	fastEvery = 500 * time.Millisecond
	slowEvery = 2 * time.Second

	// pumpHz is how often the server asks the client for a frame at the
	// "full" effects level. Nothing in the tree animates itself, so this is
	// literally ctop's frame rate.
	pumpHz = 30

	// titleEvery rate-limits window-title updates; see applyFast.
	titleEvery = 2 * time.Second
)

// Layout constants for the cpu panel. The scrub cursor has to map a
// pointer x back to a sample, which means knowing the graph pane's width so
// these are named rather than inlined. The arithmetic has to match the layout
// precisely or the crosshair points at a different sample than the readout.
const (
	contentPad = 12 // outer DockPanel padding

	// The header doubles as the window's titlebar when the system one is
	// hidden. 66px matches TitlebarStyle "tall", which centres the traffic
	// lights in a ~66pt strip, so they land on the bar's own centre line
	// rather than floating above the content.
	titlebarH = 66
	// Room on the left for those lights before our own content starts.
	trafficLights = 92
	framePad      = 12 // framed() body inset, each side
	cpuDockGap    = 8
	coreWidth     = 15 // px per core meter
)

// effects levels, dialled from the View menu.
const (
	fxOff     = 0 // repaint only when data arrives; cheapest, visibly stepped
	fxReduced = 1 // half rate
	fxFull    = 2
)

// startMotion and startTheme are what a new session begins with, resolved in
// main() from the saved prefs and the flags. Package-level because
// the shader panes read startTheme while they are being constructed. Seeding
// their palette up front avoids a frame of the wrong colours.
var (
	startMotion int32 = fxFull
	startTheme        = defaultTheme
)

// viewModeOf maps the persisted spelling to a process view mode.
func viewModeOf(s string) int {
	if s == "tree" {
		return viewTree
	}
	return viewFlat
}

// motionFPS caps the client's animation repaints to match the level. Without
// a cap the time-driven shaders run at the display's refresh rate
func motionFPS(level int32) int {
	switch level {
	case fxReduced:
		return pumpHz / 2
	case fxOff:
		return 2 // the data rate; enough that nothing looks frozen
	default:
		return pumpHz
	}
}

// app holds every node the update loop needs to touch. Handlers and
// Session.Update closures all run on the session goroutine, so none of this
// needs a lock.
type app struct {
	ses *caution.Session

	// effects is read by the pump goroutine and written by menu handlers on
	// the session goroutine, so it is atomic rather than a plain field.
	effects atomic.Int32

	st       sys.Static
	pal      float64
	themeIdx int

	backdrop *Backdrop
	plate    *Plate
	crt      *CRT

	// cpu
	cpuGraph  *Graph
	cores     *Cores
	cpuNow    *caution.Node
	cpuProbe  *caution.Node
	probeText string // last readout written, so an unchanged one costs no patch
	cpuLoad   *caution.Node
	cpuMeta   *caution.Node

	// memory
	memGraph  *Graph
	memMeter  *Meter
	swapMeter *Meter
	memHead   *caution.Node
	memBreak  *caution.Node
	swapHead  *caution.Node

	// network
	netRxGraph *Graph
	netTxGraph *Graph
	netHead    *caution.Node
	netTotals  *caution.Node

	// disk
	diskIO    *caution.Node
	diskRows  *caution.Node
	diskMeter []*Meter

	// thermal
	tempHead  *caution.Node
	tempRows  *caution.Node
	tempMeter []*Meter

	// processes
	procMode    int
	procHolder  *caution.Node
	modeSel     *caution.Node
	tree        *caution.Node
	expanded    map[string]bool // key -> is open; absent means "never touched"
	reveal      map[string]bool // ancestors a filter wants opened this pass
	table       *caution.Node
	procHead    *caution.Node
	filterField *caution.Node
	detailHead  *caution.Node
	detailCmd   *caution.Node
	procs       []sys.Proc // newest scan, unsorted
	view        []sys.Proc // filtered + sorted, what the table shows
	filter      string
	sortKey     string
	sortAsc     bool
	selected    int32
	frozen      bool // process list held still; the graphs keep running

	// Persistence: a snapshot goes to saveCh, a background writer drains to
	// the newest and writes atomically, so a burst of resizes never blocks the
	// session goroutine or races the file.
	saveCh     chan prefs
	winW, winH int

	clock     *caution.Node
	uptime    *caution.Node
	status    *caution.Node
	themeSel  *caution.Node
	lastPct   int       // last percent written to the window title
	lastTitle time.Time // when, so the Cocoa titlebar update stays rate-limited
}

func mount(s *caution.Session) *caution.Node {
	ctx, cancel := context.WithCancel(context.Background())
	col, err := sys.Start(ctx, fastEvery, slowEvery)
	if err != nil {
		cancel()
		return caution.Panel().Bg("#0b0f0d").Kids(
			caution.Label("ctop: cannot read system statistics: " + err.Error()).
				Mono().Color("#ff5c6c").Anchor(center()),
		)
	}

	pf := loadPrefs()
	a := &app{
		ses: s, st: col.Static, selected: -1,
		sortKey: pf.SortKey, sortAsc: pf.SortAsc,
		procMode: viewModeOf(pf.View),
		winW:     pf.WinW, winH: pf.WinH,
		saveCh: make(chan prefs, 8),
	}
	a.effects.Store(startMotion)
	go a.writePrefs(s)
	root := a.build()
	a.buildProcView()
	a.applyTheme(startTheme)
	a.wireCommands()

	s.OnResize(func(w, h float64) {
		// Remember the size for next launch. OnResize is client-debounced (~250ms)
		if w >= 900 && h >= 600 {
			a.winW, a.winH = int(w), int(h)
			a.persist()
		}
	})

	go func() {
		defer cancel()
		pump := time.NewTicker(time.Second / pumpHz)
		defer pump.Stop()
		sampledAt := time.Now()
		tick := 0

		for {
			select {
			case <-s.Done():
				return
			case f := <-col.Fast:
				sampledAt = time.Now()
				s.Update(func() { a.applyFast(f) })
			case sl := <-col.Slow:
				s.Update(func() { a.applySlow(sl) })
			case <-pump.C:
				fx := a.effects.Load()
				tick++
				if fx == fxOff || (fx == fxReduced && tick%2 == 1) {
					continue
				}
				// how far we are between samples, which is where the graphs
				// should have scrolled to. captured out here because the
				// closure runs later, on the session goroutine.
				frac := float64(time.Since(sampledAt)) / float64(fastEvery)
				s.Update(func() { a.animate(frac) })
			}
		}
	}()
	return root
}

func (a *app) build() *caution.Node {
	a.backdrop = NewBackdrop()

	content := caution.DockPanel().Kids(
		a.buildHeader().Dock("top").H(titlebarH),
		caution.DockPanel().Pad(contentPad).DockGap(10).Kids(
			a.buildFooter().Dock("bottom").H(22),
			a.buildCPU().Dock("top").H(214),
			caution.HStack().Gap(10).Align("stretch").Dock("top").H(178).Kids(
				a.buildMem().Fill(1.15),
				a.buildNet().Fill(1),
				a.buildDisk().Fill(1),
				a.buildThermal().Fill(1),
			),
			a.buildProcs(), // fill
		),
	)

	stage := caution.Panel().Anchor(fill()).Kids(content.Anchor(fill()))
	a.crt = Apply(stage)

	return caution.Panel().Bg("$bg").Kids(
		a.backdrop.Node().Anchor(fill()),
		stage,
	)
}

func (a *app) buildHeader() *caution.Node {
	a.plate = NewPlate()
	a.clock = mono("--:--:--", 13, "$ink")
	a.uptime = mono("up --", 11, "$inkDim")

	a.themeSel = caution.Select(themeNames(), startTheme).W(126).H(26).
		Tip("Switches design tokens and the shader phosphor together").
		OnSelect(func(i int) { a.applyTheme(i) })

	return caution.Panel().Clips().WindowDrag().Kids(
		a.plate.Node().Anchor(fill()),
		caution.HStack().Gap(16).Align("center").
			Anchor(caution.A{Left: caution.Px(trafficLights), CenterY: caution.Px(0)}).Kids(
			mono("ctop", 24, "$accent").Weight(700),
			caution.VStack().Gap(2).Kids(
				mono(a.st.Hostname, 13, "$ink").Weight(600),
				mono(a.st.Platform+"  ·  kernel "+a.st.Kernel, 10, "$inkFaint"),
			),
			caution.VStack().Gap(2).Kids(
				mono(a.st.CPUModel, 13, "$ink").Weight(600),
				mono(fmt.Sprintf("%d logical cores  ·  %d physical", a.st.Cores, a.st.Physical), 10, "$inkFaint"),
			),
		),
		caution.HStack().Gap(14).Align("center").
			Anchor(caution.A{Right: caution.Px(16), CenterY: caution.Px(0)}).Kids(
			caution.VStack().Gap(2).Align("end").Kids(a.clock, a.uptime),
			a.themeSel,
		),
	)
}

func (a *app) buildFooter() *caution.Node {
	a.status = mono("starting collectors…", 10, "$inkFaint")
	return caution.Panel().Kids(
		a.status.Anchor(caution.A{Left: caution.Px(4), CenterY: caution.Px(0)}),
	)
}

func (a *app) coresPaneWidth() float64 {
	return float64(min(a.st.Cores, coreSlots))*coreWidth + 16
}

func (a *app) cpuGraphWidth() float64 {
	w, _ := a.ses.Viewport()
	return max(w-2*contentPad-2*framePad-a.coresPaneWidth()-cpuDockGap, 1)
}

// probeCPU points the scrub cursor at a pointer position over the graph and
// reports the sample under it
func (a *app) probeCPU(x float64, toggle bool) {
	w := a.cpuGraphWidth()
	_, back, _ := a.cpuGraph.ColumnAt(min(max(x/w, 0), 1), w)
	if toggle && back == a.cpuGraph.Pinned() {
		a.clearProbe()
		return
	}
	a.cpuGraph.Pin(back, w)
	a.syncProbe()
}

const noSampleText = "· nothing recorded there yet"

// syncProbe re-points the pinned crosshair at its sample and refreshes the
// readout
func (a *app) syncProbe() {
	back := a.cpuGraph.Pinned()
	if back < 0 {
		return
	}

	if back >= a.cpuGraph.Len() {
		a.clearProbe()
		return
	}

	a.cpuGraph.Pin(back, a.cpuGraphWidth())

	v, _, ok := a.cpuGraph.Sample(back)
	if !ok {
		if a.probeText != noSampleText {
			a.probeText = noSampleText
			a.cpuProbe.SetText(noSampleText)
		}
		return
	}

	when := "now"
	if secs := float64(back) * fastEvery.Seconds(); secs > 0 {
		when = fmt.Sprintf("%.1fs ago", secs)
		if secs >= 60 {
			when = humanDur(time.Duration(back)*fastEvery) + " ago"
		}
	}

	if text := fmt.Sprintf("· %s: %.1f%%", when, v); text != a.probeText {
		a.probeText = text
		a.cpuProbe.SetText(text)
	}
}

func (a *app) clearProbe() {
	a.probeText = ""
	a.cpuGraph.Pin(-1, 0)
	a.cpuProbe.SetText("")
}

func (a *app) buildCPU() *caution.Node {
	a.cpuGraph = NewGraph(6, 100, 0, histWide)
	a.cores = NewCores(a.st.Cores)
	a.cpuNow = mono("--.-%", 26, "$ink").Weight(700)
	a.cpuLoad = mono("load  --  --  --", 11, "$inkDim")
	a.cpuMeta = mono("", 11, "$inkFaint")
	a.cpuProbe = mono("", 12, "$accent").Weight(600)

	body := caution.DockPanel().DockGap(cpuDockGap).Kids(
		caution.Panel().Dock("top").H(30).Kids(
			caution.HStack().Gap(16).Align("center").
				Anchor(caution.A{Left: caution.Px(0), CenterY: caution.Px(0)}).Kids(
				a.cpuNow, a.cpuLoad, a.cpuMeta, a.cpuProbe,
			),
		),
		caution.Panel().Dock("right").W(a.coresPaneWidth()).Kids(
			mono("per-core", 9, "$inkFaint").
				Anchor(caution.A{Left: caution.Px(2), Top: caution.Px(0)}),
			a.cores.Node().Tip("One segmented meter per logical core").
				Anchor(caution.A{Left: caution.Px(0), Right: caution.Px(0), Top: caution.Px(14), Bottom: caution.Px(0)}),
		),
		caution.Panel().Kids(
			a.cpuGraph.Node().Anchor(fill()),
			caution.Glass().Anchor(fill()).
				Tip("Total CPU across all cores, newest at the right. Click or drag to pin a reading, click again to clear").
				OnPick(func(x, _ float64, _ *caution.Node) { a.probeCPU(x, true) }).
				OnDragTo(func(x, _ float64) { a.probeCPU(x, false) }),
		),
	)
	return framed("cpu", body)
}

func (a *app) buildMem() *caution.Node {
	a.memGraph = NewGraph(5, 100, 0, histNarrow)
	a.memMeter = NewMeter(0)
	a.swapMeter = NewMeter(0)
	a.memHead = mono("--", 13, "$ink").Weight(600)
	a.memBreak = mono("", 10, "$inkFaint")
	a.swapHead = mono("swap --", 10, "$inkDim")

	body := caution.DockPanel().DockGap(5).Kids(
		a.memHead.Dock("top").H(17),
		a.memMeter.Node().Tip("Physical memory in use").Dock("top").H(11),
		a.memBreak.Dock("top").H(13),
		a.swapHead.Dock("top").H(13),
		a.swapMeter.Node().Tip("Swap in use. Heavy swap alongside low available memory is real pressure").Dock("top").H(8),
		a.memGraph.Node().Tip("Memory usage history, full scale = installed RAM"),
	)
	return framed("memory", body)
}

func (a *app) buildNet() *caution.Node {
	a.netRxGraph = NewGraph(5, 0, 64*1024, histNarrow)
	a.netTxGraph = NewGraph(5, 0, 64*1024, histNarrow)
	a.netHead = mono("--", 12, "$ink").Weight(600)
	a.netTotals = mono("", 10, "$inkFaint")

	body := caution.DockPanel().DockGap(4).Kids(
		a.netHead.Dock("top").H(16),
		a.netTotals.Dock("top").H(13),
		mono("▼ rx", 9, "$inkFaint").Dock("top").H(11),
		a.netRxGraph.Node().Tip("Autoscaling: the top of the graph is the recent peak, not a fixed rate").Dock("top").H(38),
		mono("▲ tx", 9, "$inkFaint").Dock("top").H(11),
		a.netTxGraph.Node().Tip("Autoscaling: the top of the graph is the recent peak, not a fixed rate"),
	)
	return framed("network", body)
}

func (a *app) buildDisk() *caution.Node {
	a.diskIO = mono("--", 12, "$ink").Weight(600)
	a.diskRows = caution.VStack().Gap(6).Align("stretch")

	body := caution.DockPanel().DockGap(6).Kids(
		a.diskIO.Dock("top").H(16),
		mono("read / write throughput", 9, "$inkFaint").Dock("top").H(12),
		a.diskRows,
	)
	return framed("disks", body)
}

func (a *app) buildThermal() *caution.Node {
	a.tempHead = mono("--", 12, "$ink").Weight(600).
		Tip("The hottest sensor in each zone")
	a.tempRows = caution.VStack().Gap(3).Align("stretch")

	body := caution.DockPanel().DockGap(6).Kids(
		a.tempHead.Dock("top").H(16),
		a.tempRows,
	)
	return framed("thermal", body)
}

func (a *app) buildProcs() *caution.Node {
	a.procHead = mono("--", 11, "$inkDim")
	a.expanded = map[string]bool{}
	a.reveal = map[string]bool{}

	a.filterField = caution.TextField("").Placeholder("filter by name, user, or pid…").W(280).
		Tip("Matches command, user, or a pid prefix. ⌘F jumps here, Escape reverts").
		OnInput(func(v string) {
			a.filter = v
			a.refreshProcs()
		})

	a.modeSel = caution.Select([]string{"list", "tree"}, a.procMode).W(84).
		Tip("Tree groups every process under its parent").
		OnSelect(func(i int) { a.setProcMode(i) })

	a.procHolder = caution.Panel()

	a.detailHead = mono("no process selected", 10, "$inkFaint")
	a.detailCmd = mono("click a row to see its full command line", 11, "$inkDim").
		Wrap().Selectable()

	body := caution.DockPanel().DockGap(6).Kids(
		caution.Panel().Dock("top").H(26).Kids(
			caution.HStack().Gap(10).Align("center").
				Anchor(caution.A{Left: caution.Px(0), CenterY: caution.Px(0)}).Kids(
				a.filterField, a.modeSel,
			),
			a.procHead.Anchor(caution.A{Right: caution.Px(2), CenterY: caution.Px(0)}),
		),
		caution.Panel().Dock("bottom").H(70).Bg("$panelInset").Radius(6).Clips().Kids(
			a.detailHead.Anchor(caution.A{Left: caution.Px(10), Top: caution.Px(6)}),
			a.detailCmd.Anchor(caution.A{Left: caution.Px(10), Right: caution.Px(10), Top: caution.Px(21)}),
		),
		a.procHolder,
	)
	return framed("processes", body)
}

func (a *app) setProcMode(mode int) {
	if mode == a.procMode {
		return
	}
	a.procMode = mode
	a.modeSel.SetProp("selected", mode)
	a.buildProcView()
	a.refreshProcs()
	a.persist()
}

func (a *app) refreshProcs() {
	if a.procMode == viewTree {
		if a.tree != nil {
			a.tree.SetTreeItems(a.forest())
			a.applyExpansion()
		}
		a.updateProcHead(len(a.procs))
		return
	}
	a.refreshTable()
}

func (a *app) selectedProc() (sys.Proc, bool) {
	for _, p := range a.procs {
		if p.PID == a.selected {
			return p, true
		}
	}
	return sys.Proc{}, false
}

func (a *app) setFilter(v string) {
	a.filter = v
	a.filterField.SetValueNow(v)
	a.refreshTable()
}

func (a *app) showDetail() {
	p, ok := a.selectedProc()
	if !ok {
		a.detailHead.SetText("no process selected")
		a.detailCmd.SetText("click a row to see its full command line")
		return
	}
	a.detailHead.SetText(fmt.Sprintf(
		"pid %d · ppid %d · %s · %d threads · %s rss · %.1f%% mem · %.1f%% cpu",
		p.PID, p.PPID, p.User, p.Threads, humanBytes(p.RSS), p.MemPct, p.CPU))
	a.detailCmd.SetText(p.Command)
}

// framed wraps a panel body in the standard chrome: translucent fill so the
// backdrop stays visible, a hairline border, and a corner title
func framed(title string, body *caution.Node) *caution.Node {
	return caution.Panel().Bg("$panel").Border("$edge", 1).Radius(8).Clips().Kids(
		caution.Panel().Bg("$accent").Radius(1.5).W(3).H(10).
			Anchor(caution.A{Left: caution.Px(11), Top: caution.Px(9)}),
		mono(title, 10, "$accent").Weight(600).
			Anchor(caution.A{Left: caution.Px(19), Top: caution.Px(7)}),
		body.Anchor(caution.A{Left: caution.Px(12), Right: caution.Px(12), Top: caution.Px(25), Bottom: caution.Px(10)}),
	)
}

func (a *app) writePrefs(s *caution.Session) {
	for {
		select {
		case <-s.Done():
			return
		case p := <-a.saveCh:
		drain:
			for {
				select {
				case p = <-a.saveCh:
				default:
					break drain
				}
			}
			savePrefs(p)
		}
	}
}

// persist snapshots the current choices and queues them.
func (a *app) persist() {
	view := "list"
	if a.procMode == viewTree {
		view = "tree"
	}
	select {
	case a.saveCh <- prefs{
		Theme: themes[a.themeIdx].Name, Motion: motionName(a.effects.Load()),
		View: view, SortKey: a.sortKey, SortAsc: a.sortAsc,
		WinW: a.winW, WinH: a.winH,
	}:
	default: // full: a newer snapshot is already queued, drop this one
	}
}

// wireCommands installs the menu bar and the keyboard shortcuts
func (a *app) wireCommands() {
	a.ses.SetTitle("ctop: " + a.st.Hostname)

	sortItems := []caution.MenuItem{}
	for _, c := range []struct{ title, key, combo string }{
		{"Sort by CPU", "cpu", "shift+cmd+c"},
		{"Sort by Memory", "mem", "shift+cmd+m"},
		{"Sort by Resident Size", "rss", "shift+cmd+r"},
		{"Sort by PID", "pid", "shift+cmd+i"},
		{"Sort by Name", "command", "shift+cmd+n"},
	} {
		key := c.key
		a.ses.OnKey(c.combo, func() { a.sortBy(key) })
		sortItems = append(sortItems, caution.MenuItem{
			Title: c.title, Key: c.combo, OnPick: func() { a.sortBy(key) },
		})
	}

	themeItems := make([]caution.MenuItem, len(themes))
	for i, t := range themes {
		themeItems[i] = caution.MenuItem{
			Title: t.Name, Key: strconv.Itoa(i + 1), OnPick: func() { a.applyTheme(i) },
		}
		a.ses.OnKey("cmd+"+strconv.Itoa(i+1), func() { a.applyTheme(i) })
	}

	focusFilter := func() { a.filterField.Focus() }
	clearFilter := func() { a.setFilter("") }
	toggleFreeze := func() {
		a.frozen = !a.frozen
		a.refreshTable()
	}
	revealSelected := func() {
		if n := a.procView(); n != nil {
			n.Reveal()
		}
	}
	clearSelection := func() {
		a.selected = -1
		if n := a.procView(); n != nil {
			n.SetSelectedKey("")
		}
		a.showDetail()
	}
	// Deselect drops the scrub pin and the process selection together.
	//
	// This is shift-cmd-A instead  of <esc> because that wouldn't reach the app.
	//
	// caution only dispatches registered combos when cmd/ctrl/alt is held, and
	// the terminal consumes a bare Escape itself to clear text selection
	deselect := func() {
		a.clearProbe()
		clearSelection()
	}
	a.ses.OnKey("shift+cmd+a", deselect)

	a.ses.OnKey("cmd+f", focusFilter)
	a.ses.OnKey("cmd+k", clearFilter)
	a.ses.OnKey("cmd+p", toggleFreeze)

	setFX := func(level int32) func() {
		return func() {
			a.effects.Store(level)
			a.refreshStatus()
			a.persist()
		}
	}

	a.ses.SetMenu(
		caution.Menu{Title: "View", Items: []caution.MenuItem{
			{Title: "Theme", Items: themeItems},
			{Sep: true},
			{Title: "Motion", Items: []caution.MenuItem{
				{Title: "Full (30 fps)", OnPick: setFX(fxFull)},
				{Title: "Reduced (15 fps)", OnPick: setFX(fxReduced)},
				{Title: "Off (repaint only on new data)", OnPick: setFX(fxOff)},
			}},
			{Sep: true},
			{Title: "Freeze Process List", Key: "p", OnPick: toggleFreeze},
		}},
		caution.Menu{Title: "Processes", Items: append(sortItems,
			caution.MenuItem{Sep: true},
			caution.MenuItem{Title: "Focus Filter", Key: "f", OnPick: focusFilter},
			caution.MenuItem{Title: "Clear Filter", Key: "k", OnPick: clearFilter},
			caution.MenuItem{Sep: true},
			caution.MenuItem{Title: "Scroll to Selection", OnPick: revealSelected},
			caution.MenuItem{Title: "Deselect", Key: "shift+cmd+a", OnPick: deselect},
		)},
	)
}

func (a *app) sortBy(key string) {
	a.sortKey = key
	switch key {
	case "cpu", "mem", "rss", "thr":
		a.sortAsc = false
	default:
		a.sortAsc = true
	}
	dir := "desc"
	if a.sortAsc {
		dir = "asc"
	}
	a.table.SetProp("sortKey", key).SetProp("sortDir", dir)
	a.refreshTable()
	a.persist()
}

func (a *app) applyFast(f sys.Fast) {
	a.cpuGraph.Push(f.CPUTotal)
	a.syncProbe() // the pin aged by one sample. refresh its readout
	a.cores.Set(f.CPUCores)
	a.cpuNow.SetText(fmt.Sprintf("%.1f%%", f.CPUTotal))
	a.cpuNow.SetColor(loadColor(f.CPUTotal / 100))
	a.cpuLoad.SetText(fmt.Sprintf("load %.2f  %.2f  %.2f", f.Load[0], f.Load[1], f.Load[2]))
	a.cpuMeta.SetText(fmt.Sprintf("· %d cores · %s", a.st.Cores, a.st.CPUModel))

	if f.MemTotal > 0 {
		used := float64(f.MemUsed) / float64(f.MemTotal)
		a.memGraph.Push(used * 100)
		a.memMeter.Set(used)
		a.memHead.SetText(fmt.Sprintf("%s / %s   %.1f%%",
			humanBytes(f.MemUsed), humanBytes(f.MemTotal), used*100))
		a.memHead.SetColor(loadColor(used))
		a.memBreak.SetText(fmt.Sprintf("wired %s · active %s · available %s",
			humanBytes(f.MemWired), humanBytes(f.MemActive), humanBytes(f.MemAvail)))
	}
	if f.SwapTotal > 0 {
		sw := float64(f.SwapUsed) / float64(f.SwapTotal)
		a.swapMeter.Set(sw)
		a.swapHead.SetText(fmt.Sprintf("swap %s / %s  (%.0f%%)",
			humanBytes(f.SwapUsed), humanBytes(f.SwapTotal), sw*100))
	} else {
		a.swapHead.SetText("swap disabled")
	}

	a.netRxGraph.Push(f.NetRx)
	a.netTxGraph.Push(f.NetTx)
	iface := f.NetIface
	if iface == "" {
		iface = "n/a"
	}
	a.netHead.SetText(fmt.Sprintf("▼ %s   ▲ %s", humanRate(f.NetRx), humanRate(f.NetTx)))
	a.netTotals.SetText(fmt.Sprintf("%s · %s in / %s out since boot",
		iface, humanBytes(f.NetRxTotal), humanBytes(f.NetTxTotal)))

	a.diskIO.SetText(fmt.Sprintf("▼ %s   ▲ %s", humanRate(f.DiskRead), humanRate(f.DiskWrite)))

	if pct := int(f.CPUTotal + 0.5); pct != a.lastPct && f.Time.Sub(a.lastTitle) >= titleEvery {
		a.lastPct, a.lastTitle = pct, f.Time
		a.ses.SetTitle(fmt.Sprintf("ctop: %s, %d%% cpu", a.st.Hostname, pct))
	}

	if a.effects.Load() == fxOff {
		a.animate(0)
		a.cores.Ease(1)
		a.memMeter.Ease(1)
		a.swapMeter.Ease(1)
		for _, m := range a.diskMeter {
			m.Ease(1)
		}
		for _, m := range a.tempMeter {
			m.Ease(1)
		}
	}

	a.clock.SetText(f.Time.Format("15:04:05"))
	a.uptime.SetText("up " + humanDur(f.Uptime))
	a.refreshStatus()
}

func (a *app) animate(frac float64) {
	const alpha = 0.22 // per-tick approach; ~8 ticks to close a gap visibly

	a.cpuGraph.Shift(frac)
	a.memGraph.Shift(frac)
	a.netRxGraph.Shift(frac)
	a.netTxGraph.Shift(frac)

	a.syncProbe()

	a.cores.Ease(alpha)
	a.memMeter.Ease(alpha)
	a.swapMeter.Ease(alpha)
	for _, m := range a.diskMeter {
		m.Ease(alpha)
	}
	for _, m := range a.tempMeter {
		m.Ease(alpha)
	}
}

func (a *app) refreshStatus() {
	fps := "motion off"
	switch a.effects.Load() {
	case fxFull:
		fps = fmt.Sprintf("%d fps", pumpHz)
	case fxReduced:
		fps = fmt.Sprintf("%d fps", pumpHz/2)
	}
	state := fmt.Sprintf("sampling %v · processes %v · %s · %d graph samples retained",
		fastEvery, slowEvery, fps, histWide)
	if a.frozen {
		state = "process list FROZEN (⌘P to resume) · " + state
	}
	a.status.SetText(state)
}

func (a *app) applySlow(s sys.Slow) {
	a.setDisks(s.Disks)
	a.setTemps(s.Temps)
	if a.frozen {
		return // reading a row is impossible if it moves out from under you
	}
	a.procs = s.Procs
	a.refreshProcs()
}

func (a *app) setDisks(disks []sys.Disk) {
	if len(disks) != len(a.diskMeter) {
		a.diskRows.Clear()
		a.diskMeter = a.diskMeter[:0]
		for _, d := range disks {
			m := NewMeter(0)
			m.SetPal(a.pal)
			a.diskMeter = append(a.diskMeter, m)
			a.diskRows.Add(caution.VStack().Gap(3).Align("stretch").Kids(
				mono(diskCaption(d), 10, "$inkDim"),
				m.Node().Tip(diskTip(d)).Fixed(8),
			))
		}
	}
	for i, d := range disks {
		if i < len(a.diskMeter) {
			a.diskMeter[i].Set(d.Percent / 100)
			a.diskMeter[i].Node().Tip(diskTip(d))
		}
		if row := nthChild(a.diskRows, i); row != nil {
			if cap := nthChild(row, 0); cap != nil {
				cap.SetText(diskCaption(d))
			}
		}
	}
}

func (a *app) setTemps(temps []sys.Temp) {
	if len(temps) == 0 {
		a.tempHead.SetText("no sensors")
		a.tempRows.Clear()
		a.tempMeter = a.tempMeter[:0]
		return
	}
	a.tempHead.SetText(fmt.Sprintf("%.0f °C peak", temps[0].C))

	rows := sys.GroupTemps(temps)
	if len(rows) != len(a.tempMeter) {
		a.tempRows.Clear()
		a.tempMeter = a.tempMeter[:0]
		for _, t := range rows {
			m := NewMeter(0)
			m.SetPal(a.pal)
			a.tempMeter = append(a.tempMeter, m)
			a.tempRows.Add(caution.Panel().Fixed(14).Kids(
				mono(tempCaption(t), 10, "$inkDim").
					Anchor(caution.A{Left: caution.Px(0), CenterY: caution.Px(0)}),
				m.Node().Tip(tempTip(t)).Anchor(caution.A{
					Left: caution.Px(tempLabelW), Right: caution.Px(0),
					Top: caution.Px(3), Bottom: caution.Px(3),
				}),
			))
		}
	}
	for i, t := range rows {
		frac := tempFrac(t)
		a.tempMeter[i].Set(frac)
		a.tempMeter[i].Node().Tip(tempTip(t))
		if row := nthChild(a.tempRows, i); row != nil {
			if cap := nthChild(row, 0); cap != nil {
				cap.SetText(tempCaption(t))
				cap.SetColor(loadColor(frac))
			}
		}
	}
}

func tempFrac(t sys.Temp) float64 {
	ceiling := t.Crit
	if ceiling <= 0 {
		ceiling = t.High
	}
	if ceiling <= 0 {
		ceiling = 100
	}
	return min(max(t.C/ceiling, 0), 1)
}

const tempLabelW = 76

func tempCaption(t sys.Temp) string {
	return fmt.Sprintf("%-4s  %.0f °C", t.Class, t.C)
}

func tempTip(t sys.Temp) string {
	s := fmt.Sprintf("%s reads %.1f °C", t.Label, t.C)
	if t.Crit > 0 {
		s += fmt.Sprintf(", critical at %.0f °C", t.Crit)
	}
	return s + ". Sensor names come from the hardware; ctop groups them by what they sit on"
}

func diskTip(d sys.Disk) string {
	return fmt.Sprintf("%s on %s (%s), %s free of %s",
		d.Mount, d.Device, d.FSType, humanBytes(d.Total-d.Used), humanBytes(d.Total))
}

func diskCaption(d sys.Disk) string {
	return fmt.Sprintf("%s   %s / %s   %.0f%%",
		d.Mount, humanBytes(d.Used), humanBytes(d.Total), d.Percent)
}

func nthChild(n *caution.Node, i int) *caution.Node {
	kids := n.Children()
	if i < 0 || i >= len(kids) {
		return nil
	}
	return kids[i]
}

func (a *app) refreshTable() {
	if a.table == nil || a.procMode != viewFlat {
		return
	}
	q := strings.ToLower(strings.TrimSpace(a.filter))
	a.view = a.view[:0]
	for _, p := range a.procs {
		if q != "" && !matches(p, q) {
			continue
		}
		a.view = append(a.view, p)
	}

	asc := a.sortAsc
	sort.SliceStable(a.view, func(i, j int) bool {
		x, y := a.view[i], a.view[j]
		var less bool
		switch a.sortKey {
		case "pid":
			less = x.PID < y.PID
		case "user":
			less = x.User < y.User
		case "mem":
			less = x.MemPct < y.MemPct
		case "rss":
			less = x.RSS < y.RSS
		case "thr":
			less = x.Threads < y.Threads
		case "command":
			less = strings.ToLower(x.Command) < strings.ToLower(y.Command)
		default: // cpu
			less = x.CPU < y.CPU
		}
		if asc {
			return less
		}
		return !less
	})

	a.table.SetRowCount(len(a.view)).RefreshRows()

	if a.selected >= 0 {
		a.table.SetSelectedKey(strconv.Itoa(int(a.selected)))
	}
	a.showDetail()

	a.updateProcHead(len(a.view))
}

// updateProcHead reports how many processes are on screen out of how many
// exist
func (a *app) updateProcHead(shown int) {
	var busy float64
	for _, p := range a.procs {
		busy += p.CPU
	}
	label := fmt.Sprintf("%d processes · %.0f%% total cpu", shown, busy)
	if shown != len(a.procs) {
		label = fmt.Sprintf("%d of %d processes · %.0f%% total cpu", shown, len(a.procs), busy)
	}
	a.procHead.SetText(label)
}

func matches(p sys.Proc, q string) bool {
	return strings.Contains(strings.ToLower(p.Command), q) ||
		strings.Contains(strings.ToLower(p.User), q) ||
		strings.HasPrefix(strconv.Itoa(int(p.PID)), q)
}

func (a *app) rows(start, end int) [][]string {
	if start < 0 {
		start = 0
	}
	if end >= len(a.view) {
		end = len(a.view) - 1
	}
	out := make([][]string, 0, max(end-start+1, 0))
	for i := start; i <= end; i++ {
		out = append(out, a.flatCells(a.view[i]))
	}
	return out
}

func (a *app) applyTheme(i int) {
	if i < 0 || i >= len(themes) {
		return
	}
	t := themes[i]
	a.themeIdx = i
	a.pal = t.Pal
	a.ses.SetTheme(t.Tokens)
	a.themeSel.SetProp("selected", i)

	a.backdrop.SetPal(t.Pal)
	a.plate.SetPal(t.Pal)
	a.crt.SetPal(t.Pal)
	a.cpuGraph.SetPal(t.Pal)
	a.cores.SetPal(t.Pal)
	a.memGraph.SetPal(t.Pal)
	a.memMeter.SetPal(t.Pal)
	a.swapMeter.SetPal(t.Pal)
	a.netRxGraph.SetPal(t.Pal)
	a.netTxGraph.SetPal(t.Pal)
	for _, m := range a.diskMeter {
		m.SetPal(t.Pal)
	}
	for _, m := range a.tempMeter {
		m.SetPal(t.Pal)
	}
	a.persist()
}

func loadColor(frac float64) string {
	switch {
	case frac > 0.85:
		return "$crit"
	case frac > 0.6:
		return "$warn"
	default:
		return "$ink"
	}
}

func mono(text string, size float64, color string) *caution.Node {
	return caution.Label(text).Mono().FontSize(size).Color(color)
}

func fill() caution.A {
	return caution.A{Left: caution.Px(0), Right: caution.Px(0), Top: caution.Px(0), Bottom: caution.Px(0)}
}

func center() caution.A {
	return caution.A{CenterX: caution.Px(0), CenterY: caution.Px(0)}
}

func main() {
	serve := flag.Bool("serve", false, "serve browsers over HTTP instead of opening a window")
	addr := flag.String("addr", ":8080", `listen address for -serve: ":8080" or "unix:/path.sock"`)
	motion := flag.String("motion", "", `animation: "full" (30fps), "reduced" (15fps), or "off" (repaint only on new data); default is whatever was last used`)
	systemBar := flag.Bool("system-titlebar", false, "keep the macOS titlebar instead of drawing our own")
	shot := flag.String("shot", "", "render one frame offscreen to this PNG and exit")
	settle := flag.Float64("settle", 1200, "milliseconds to let data arrive before -shot")
	clicks := flag.String("clicks", "", `synthetic input before -shot, e.g. "600,300@200"`)
	flag.Parse()

	pf := loadPrefs()
	startTheme, _ = themeIndex(pf.Theme)
	startMotion, _ = motionLevel(pf.Motion)
	if *motion != "" {
		level, ok := motionLevel(*motion)
		if !ok {
			log.Fatalf("ctop: -motion must be full, reduced, or off (got %q)", *motion)
		}
		startMotion = level
	}

	w, h := pf.WinW, pf.WinH
	if *shot != "" {
		w, h = 1440, 900
	}

	caution.ServeClient()
	if *serve && !native.InBundle() {
		log.Printf("ctop serving on %s", *addr)
		log.Fatal(caution.Serve(*addr, mount))
	}
	if err := native.Run(mount, native.Options{
		Title: "ctop", W: w, H: h, MaxFPS: motionFPS(startMotion),
		CustomTitlebar: !*systemBar, TitlebarStyle: "tall",
		Shot: *shot, SettleMs: *settle, Clicks: *clicks,
	}); err != nil {
		log.Fatal(err)
	}
}
