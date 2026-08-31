package main

import (
	"sort"
	"strconv"
	"strings"

	caution "github.com/nullentropy/caution/go"
	"github.com/nullentropy/ctop/sys"
)

const (
	viewFlat = 0
	viewTree = 1
)

// procCols is the flat table's column set. "load" is an in-cell progress
// column: cells are 0..1 floats and the client draws the bar, so a bar per row
// costs a string per row rather than a node per row.
func procCols() []caution.Col {
	return []caution.Col{
		{Key: "pid", Title: "PID", Width: 62},
		{Key: "user", Title: "USER", Width: 92},
		{Key: "cpu", Title: "CPU%", Width: 60},
		{Key: "load", Title: "", Width: 74, Kind: "progress"},
		{Key: "mem", Title: "MEM%", Width: 62},
		{Key: "rss", Title: "RSS", Width: 82},
		{Key: "thr", Title: "THR", Width: 48},
		{Key: "command", Title: "COMMAND", Weight: 1},
	}
}

// treeCols leads with the command, because that is the column the disclosure
// triangles and indentation live in. The hierarchy has to be the thing you
// read first or the outline is unreadable.
func treeCols() []caution.Col {
	return []caution.Col{
		{Key: "command", Title: "COMMAND", Weight: 1},
		{Key: "pid", Title: "PID", Width: 62},
		{Key: "user", Title: "USER", Width: 92},
		{Key: "cpu", Title: "CPU%", Width: 60},
		{Key: "load", Title: "", Width: 74, Kind: "progress"},
		{Key: "mem", Title: "MEM%", Width: 62},
		{Key: "rss", Title: "RSS", Width: 82},
	}
}

// loadCell renders a CPU percentage for the progress column, scaled to one
// core the way htop does
func (a *app) loadCell(cpu float64) string {
	return strconv.FormatFloat(min(max(cpu/100, 0), 1), 'f', 3, 64)
}

func (a *app) flatCells(p sys.Proc) []string {
	return []string{
		strconv.Itoa(int(p.PID)),
		p.User,
		strconv.FormatFloat(p.CPU, 'f', 1, 64),
		a.loadCell(p.CPU),
		strconv.FormatFloat(p.MemPct, 'f', 1, 64),
		humanBytes(p.RSS),
		strconv.Itoa(int(p.Threads)),
		p.Command,
	}
}

func (a *app) treeCells(p sys.Proc) []string {
	return []string{
		p.Command,
		strconv.Itoa(int(p.PID)),
		p.User,
		strconv.FormatFloat(p.CPU, 'f', 1, 64),
		a.loadCell(p.CPU),
		strconv.FormatFloat(p.MemPct, 'f', 1, 64),
		humanBytes(p.RSS),
	}
}

// buildProcView creates the node for the current mode and installs it in the
// holder. Handlers are attached here rather than shared, because Table and
// Tree differ in which of them apply.
func (a *app) buildProcView() {
	a.procHolder.Clear()

	if a.procMode == viewTree {
		a.tree = caution.Tree(treeCols(), a.forest()).RowHeight(19).
			OnRowSelectKey(a.selectByKey).
			OnRowToggle(func(key string, expanded bool) {
				// Remember what the user closed, so the next refresh does not
				// helpfully re-open it.
				a.expanded[key] = expanded
			}).
			Context(a.procContext()...)
		// The holder is a plain panel, so the view has to be anchored to fill
		// it, since it is no longer a dock child getting bounds for free.
		a.procHolder.Add(a.tree.Anchor(fill()))
		a.applyExpansion()
		return
	}

	a.table = caution.Table(procCols(), 0).RowHeight(19).
		RowKey(0).
		SetProp("sortKey", a.sortKey).SetProp("sortDir", a.sortDir()).
		RowsFunc(a.rows).
		OnSort(func(key string, asc bool) {
			a.sortKey, a.sortAsc = key, asc
			a.refreshTable()
		}).
		OnRowSelectKey(a.selectByKey).
		Context(a.procContext()...)
	a.procHolder.Add(a.table.Anchor(fill()))
	a.refreshTable()
}

// procView is whichever of the two shapes is currently mounted.
func (a *app) procView() *caution.Node {
	if a.procMode == viewTree {
		return a.tree
	}
	return a.table
}

func (a *app) selectByKey(key string, _ int) {
	pid, err := strconv.Atoi(key)
	if err != nil {
		return
	}
	a.selected = int32(pid)
	a.showDetail()
}

// procContext is the right-click menu, shared by both shapes. It acts on the
// selection rather than the row under the cursor: a context pick carries no
// row, and right-clicking does not move the selection.
func (a *app) procContext() []caution.ContextItem {
	return []caution.ContextItem{
		{Title: "Filter to selected process", OnPick: func() {
			if p, ok := a.selectedProc(); ok {
				a.setFilter(strconv.Itoa(int(p.PID)))
			}
		}},
		{Title: "Filter to selected user", OnPick: func() {
			if p, ok := a.selectedProc(); ok && p.User != "" {
				a.setFilter(p.User)
			}
		}},
		{Sep: true},
		{Title: "Clear filter", OnPick: func() { a.setFilter("") }},
	}
}

func (a *app) sortDir() string {
	if a.sortAsc {
		return "asc"
	}
	return "desc"
}

// forest builds the parent/child structure from PPID.
//
// With a filter active it keeps every match plus that match's whole ancestor
// chain, so a hit is shown where it actually lives rather than uprooted, and
// those ancestors are auto-expanded to reveal it.
func (a *app) forest() []caution.TreeItem {
	byPID := make(map[int32]sys.Proc, len(a.procs))
	for _, p := range a.procs {
		byPID[p.PID] = p
	}

	keep := map[int32]bool{}
	q := strings.ToLower(strings.TrimSpace(a.filter))
	if q != "" {
		for _, p := range a.procs {
			if !matches(p, q) {
				continue
			}
			keep[p.PID] = true
			for pid := p.PPID; pid != 0; {
				parent, ok := byPID[pid]
				if !ok || keep[pid] {
					break
				}
				keep[pid] = true
				a.reveal[strconv.Itoa(int(pid))] = true
				pid = parent.PPID
			}
		}
	}

	kids := map[int32][]sys.Proc{}
	var roots []sys.Proc
	for _, p := range a.procs {
		if q != "" && !keep[p.PID] {
			continue
		}
		// A process whose parent is gone (or filtered away) is a root here,
		// otherwise the whole subtree would vanish with it.
		if _, ok := byPID[p.PPID]; !ok || p.PPID == 0 || (q != "" && !keep[p.PPID]) {
			roots = append(roots, p)
			continue
		}
		kids[p.PPID] = append(kids[p.PPID], p)
	}

	a.sortProcs(roots)
	for pid := range kids {
		a.sortProcs(kids[pid])
	}

	// Depth is bounded by the real process hierarchy, but guard anyway: a
	// corrupt ppid cycle would otherwise recurse forever
	var build func(p sys.Proc, depth int) caution.TreeItem
	build = func(p sys.Proc, depth int) caution.TreeItem {
		item := caution.TreeItem{Key: strconv.Itoa(int(p.PID)), Cells: a.treeCells(p)}
		if depth < 24 {
			for _, k := range kids[p.PID] {
				item.Kids = append(item.Kids, build(k, depth+1))
			}
		}
		return item
	}

	out := make([]caution.TreeItem, 0, len(roots))
	for _, r := range roots {
		out = append(out, build(r, 0))
	}
	return out
}

// applyExpansion opens rows the user has not explicitly closed. A process tree
// that starts fully collapsed shows two rows (launchd and the kernel), which
// is useless, so new parents default to open.
func (a *app) applyExpansion() {
	if a.tree == nil {
		return
	}
	var open, shut []string
	var walk func(items []caution.TreeItem)
	walk = func(items []caution.TreeItem) {
		for _, it := range items {
			if len(it.Kids) > 0 {
				if closed, seen := a.expanded[it.Key]; seen && !closed {
					shut = append(shut, it.Key)
				} else {
					open = append(open, it.Key)
				}
				walk(it.Kids)
			}
		}
	}
	walk(a.forest())
	if len(open) > 0 {
		a.tree.Expand(open...)
	}
	if len(shut) > 0 {
		a.tree.Collapse(shut...)
	}
	clear(a.reveal)
}

func (a *app) sortProcs(ps []sys.Proc) {
	asc := a.sortAsc
	sort.SliceStable(ps, func(i, j int) bool {
		x, y := ps[i], ps[j]
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
		default: // cpu, and the "load" bar column which is the same number
			less = x.CPU < y.CPU
		}
		if asc {
			return less
		}
		return !less
	})
}
