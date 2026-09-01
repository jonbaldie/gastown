package feed

import "github.com/charmbracelet/bubbles/key"

// KeyMap defines the key bindings for the feed TUI.
type KeyMap struct {
	navigationKeys
	panelKeys
	actionKeys
	problemKeys
	searchKeys
	generalKeys
}

type navigationKeys struct {
	Up       key.Binding
	Down     key.Binding
	PageUp   key.Binding
	PageDown key.Binding
	Top      key.Binding
	Bottom   key.Binding
}

type panelKeys struct {
	Tab         key.Binding
	ShiftTab    key.Binding
	FocusTree   key.Binding
	FocusConvoy key.Binding
	FocusFeed   key.Binding
}

type actionKeys struct {
	Enter   key.Binding
	Expand  key.Binding
	Refresh key.Binding
}

type problemKeys struct {
	ToggleProblems key.Binding
	Nudge          key.Binding
	Handoff        key.Binding
}

type searchKeys struct {
	Search      key.Binding
	Filter      key.Binding
	ClearFilter key.Binding
}

type generalKeys struct {
	Help key.Binding
	Quit key.Binding
}

// DefaultKeyMap returns the default key bindings.
func DefaultKeyMap() KeyMap {
	return KeyMap{
		navigationKeys: navigationKeys{
			Up:       newKeyBinding([]string{"up", "k"}, "↑/k", "up"),
			Down:     newKeyBinding([]string{"down", "j"}, "↓/j", "down"),
			PageUp:   newKeyBinding([]string{"pgup", "ctrl+u"}, "pgup", "page up"),
			PageDown: newKeyBinding([]string{"pgdown", "ctrl+d"}, "pgdn", "page down"),
			Top:      newKeyBinding([]string{"home", "g"}, "g", "top"),
			Bottom:   newKeyBinding([]string{"end", "G"}, "G", "bottom"),
		},
		panelKeys: panelKeys{
			Tab:         newKeyBinding([]string{"tab"}, "tab", "switch panel"),
			ShiftTab:    newKeyBinding([]string{"shift+tab"}, "S-tab", "prev panel"),
			FocusTree:   newKeyBinding([]string{"1"}, "1", "agent tree"),
			FocusConvoy: newKeyBinding([]string{"2"}, "2", "convoys"),
			FocusFeed:   newKeyBinding([]string{"3"}, "3", "event feed"),
		},
		actionKeys: actionKeys{
			Enter:   newKeyBinding([]string{"enter"}, "enter", "expand/details"),
			Expand:  newKeyBinding([]string{"o", "l"}, "o", "toggle expand"),
			Refresh: newKeyBinding([]string{"R"}, "R", "refresh"),
		},
		problemKeys: problemKeys{
			ToggleProblems: newKeyBinding([]string{"p"}, "p", "toggle problems view"),
			Nudge:          newKeyBinding([]string{"n"}, "n", "nudge agent"),
			Handoff:        newKeyBinding([]string{"h"}, "h", "handoff agent"),
		},
		searchKeys: searchKeys{
			Search:      newKeyBinding([]string{"/"}, "/", "search"),
			Filter:      newKeyBinding([]string{"f"}, "f", "filter"),
			ClearFilter: newKeyBinding([]string{"esc"}, "esc", "clear"),
		},
		generalKeys: generalKeys{
			Help: newKeyBinding([]string{"?"}, "?", "help"),
			Quit: newKeyBinding([]string{"q", "ctrl+c"}, "q", "quit"),
		},
	}
}

func newKeyBinding(keys []string, keyHelp, description string) key.Binding {
	return key.NewBinding(
		key.WithKeys(keys...),
		key.WithHelp(keyHelp, description),
	)
}

// ShortHelp returns key bindings for the short help view.
func (k KeyMap) ShortHelp() []key.Binding {
	return []key.Binding{k.Up, k.Down, k.Tab, k.ToggleProblems, k.Search, k.Quit, k.Help}
}

// FullHelp returns key bindings for the full help view.
func (k KeyMap) FullHelp() [][]key.Binding {
	return [][]key.Binding{
		{k.Up, k.Down, k.PageUp, k.PageDown, k.Top, k.Bottom},
		{k.Tab, k.FocusTree, k.FocusConvoy, k.FocusFeed, k.Enter, k.Expand},
		{k.ToggleProblems, k.Nudge, k.Handoff},
		{k.Search, k.Filter, k.ClearFilter, k.Refresh},
		{k.Help, k.Quit},
	}
}
