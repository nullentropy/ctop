package main

type theme struct {
	Name   string
	Pal    float64 // selects the shader phosphor (see pal3 in shaders.go)
	Tokens map[string]string
}

// defaultTheme is the one a session starts in as an index rather than by
// reordering the slice, so the cmd-N shortcuts stay bound to the same themes
// as new ones are appended.
const defaultTheme = 3 // vaporwave

var themes = []theme{{
	Name: "phosphor",
	Pal:  0,
	Tokens: map[string]string{
		"bg": "#04070600", "ink": "#b8f5cf", "inkDim": "#5fae83", "inkFaint": "#35704f",
		"panel": "#06120de6", "panelAlt": "#08160fe6", "panelInset": "#04100be6",
		"edge": "#144c33", "edgeSoft": "#0e3524", "titlebar": "#0a1d15",
		"control": "#0a1f16", "controlEdge": "#1a5c3d", "accent": "#29ff8c",
		"warn": "#f2e64d", "crit": "#ff4258",
	},
}, {
	Name: "amber",
	Pal:  1,
	Tokens: map[string]string{
		"bg": "#07050200", "ink": "#ffdcab", "inkDim": "#b98a4e", "inkFaint": "#7a5a30",
		"panel": "#150e04e6", "panelAlt": "#180f05e6", "panelInset": "#100a03e6",
		"edge": "#5a3c12", "edgeSoft": "#3d280c", "titlebar": "#201505",
		"control": "#221606", "controlEdge": "#6b480f", "accent": "#ffb340",
		"warn": "#ff7a1f", "crit": "#ff3829",
	},
}, {
	Name: "ice",
	Pal:  2,
	Tokens: map[string]string{
		"bg": "#03060b00", "ink": "#c2e6ff", "inkDim": "#6d9dc4", "inkFaint": "#426a8a",
		"panel": "#071322e6", "panelAlt": "#08172ae6", "panelInset": "#05101ce6",
		"edge": "#14456e", "edgeSoft": "#0e3050", "titlebar": "#0a1e33",
		"control": "#0b2138", "controlEdge": "#1b5a8c", "accent": "#3ff0ff",
		"warn": "#4d8cff", "crit": "#b85cff",
	},
}, {
	Name: "vaporwave",
	Pal:  3,
	Tokens: map[string]string{
		"bg": "#0a041800", "ink": "#ffd9f4", "inkDim": "#b98fd8", "inkFaint": "#7d5aa0",
		"panel": "#170a2ed4", "panelAlt": "#1c0d36d4", "panelInset": "#120823d4",
		"edge": "#6b2a8f", "edgeSoft": "#43195c", "titlebar": "#24103d",
		"control": "#26113f", "controlEdge": "#8f3ab5", "accent": "#00ccff",
		"warn": "#b967ff", "crit": "#ff3ea5",
	},
}, {
	Name: "runner",
	Pal:  4,
	Tokens: map[string]string{
		"bg": "#eef1f400", "ink": "#12181d", "inkDim": "#57646f", "inkFaint": "#8d9ca8",
		"panel": "#f0f3f6e6", "panelAlt": "#e8edf1e6", "panelInset": "#e2e8ede6",
		"edge": "#c3ced6", "edgeSoft": "#d8e0e6", "titlebar": "#ffffff",
		"control": "#ffffff", "controlEdge": "#bcc8d1", "accent": "#1b232a",
		"warn": "#d95f00", "crit": "#d10a22",
	},
}}

func themeNames() []string {
	out := make([]string, len(themes))
	for i, t := range themes {
		out[i] = t.Name
	}
	return out
}
