package memory

import (
	"fmt"
)

// RefusalText says why a runtime wasn't started, what has the memory and what to do. The figures
// are the host's reserve, what is free and what LocalBase has reserved, never which models. Anything
// else keeps the general sentence.
func RefusalText(e *AdmissionError, subject string) string {
	var free string
	requested := e.RequestedBytes
	if g := e.Diagnostics; g != nil {
		requested = g.Requested
		if g.EffectiveAvailable >= 0 && g.Reserve >= 0 {
			free = fmt.Sprintf(", and %s is free after keeping %s for your computer", GiBText(max(0, g.EffectiveAvailable-g.Reserve)), GiBText(g.Reserve))
		}
	}
	reserved := "LocalBase's loaded models have " + GiBText(e.ReservedBytes) + " reserved"
	action := "Free memory from a loaded model or close other apps, then try again."
	if e.ReservedBytes == 0 {
		reserved = ""
		action = "LocalBase has nothing loaded, so other apps are using this memory. Close other apps, then try again."
	}
	switch e.Reason {
	case "memory-pressure":
		switch {
		case requested > 0 && reserved != "":
			action = "It needs " + GiBText(requested) + ", and " + reserved + ". " + action
		case requested > 0:
			action = "It needs " + GiBText(requested) + ". " + action
		case reserved != "":
			action = reserved + ". " + action
		}
		return fmt.Sprintf("This computer is under memory pressure, so %s wasn't started. %s", subject, action)
	case "system-memory":
		if reserved != "" {
			action = reserved + ". " + action
		}
		if requested <= 0 { // Not known: the job's failure didn't carry it.
			return fmt.Sprintf("Not enough free memory to start %s%s. %s", subject, free, action)
		}
		return fmt.Sprintf("Not enough free memory to start %s: it needs %s%s. %s", subject, GiBText(requested), free, action)
	}
	return "Insufficient available memory to start the requested runtime. Please try again shortly."
}
