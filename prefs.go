package main

import (
	"encoding/json"
	"os"
	"path/filepath"
)

type prefs struct {
	Theme   string `json:"theme"`
	Motion  string `json:"motion"`
	View    string `json:"view"`
	SortKey string `json:"sortKey"`
	SortAsc bool   `json:"sortAsc"`
	WinW    int    `json:"winW"`
	WinH    int    `json:"winH"`
}

func defaultPrefs() prefs {
	return prefs{
		Theme: themes[defaultTheme].Name, Motion: "full", View: "list",
		SortKey: "cpu", SortAsc: false, WinW: 1440, WinH: 900,
	}
}

func prefsPath() string {
	dir, err := os.UserConfigDir()
	if err != nil || dir == "" {
		dir = os.TempDir()
	}
	return filepath.Join(dir, "ctop", "prefs.json")
}

func loadPrefs() prefs {
	p := defaultPrefs()
	if data, err := os.ReadFile(prefsPath()); err == nil {
		_ = json.Unmarshal(data, &p) // best-effort; defaults survive a parse error
	}
	d := defaultPrefs()
	if _, ok := themeIndex(p.Theme); !ok {
		p.Theme = d.Theme
	}
	if _, ok := motionLevel(p.Motion); !ok {
		p.Motion = d.Motion
	}
	if p.View != "list" && p.View != "tree" {
		p.View = d.View
	}
	if !sortableKeys[p.SortKey] {
		p.SortKey, p.SortAsc = d.SortKey, d.SortAsc
	}
	if p.WinW < 900 {
		p.WinW = d.WinW
	}
	if p.WinH < 600 {
		p.WinH = d.WinH
	}
	return p
}

func savePrefs(p prefs) {
	path := prefsPath()
	if os.MkdirAll(filepath.Dir(path), 0o755) != nil {
		return
	}
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return
	}
	tmp := path + ".tmp"
	if os.WriteFile(tmp, data, 0o644) != nil {
		return
	}
	_ = os.Rename(tmp, path)
}

var sortableKeys = map[string]bool{
	"pid": true, "user": true, "cpu": true, "mem": true,
	"rss": true, "thr": true, "command": true,
}

func themeIndex(name string) (int, bool) {
	for i, t := range themes {
		if t.Name == name {
			return i, true
		}
	}
	return defaultTheme, false
}

func motionLevel(s string) (int32, bool) {
	switch s {
	case "full":
		return fxFull, true
	case "reduced":
		return fxReduced, true
	case "off":
		return fxOff, true
	}
	return fxFull, false
}

func motionName(level int32) string {
	switch level {
	case fxReduced:
		return "reduced"
	case fxOff:
		return "off"
	default:
		return "full"
	}
}
