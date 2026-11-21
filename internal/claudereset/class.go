package claudereset

import "strings"

// ClassOf classifies a grant by the windows it clears. Only grants restoring
// the weekly window (optionally with the Fable window), or only the Fable
// window, are understood. It also reports exactly which windows are cleared.
func ClassOf(clears []string) (class Class, weekly, fable bool) {
	other := false
	for _, name := range clears {
		switch strings.TrimSpace(name) {
		case WindowWeekly:
			weekly = true
		case WindowFable:
			fable = true
		default:
			other = true
		}
	}
	switch {
	case other || (!weekly && !fable):
		return ClassUnsupported, false, false
	case weekly:
		return ClassWeekly, weekly, fable
	}
	return ClassFable, false, true
}
