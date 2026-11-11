package conf

// RestartDecision is whether apply restarts the managed service, or why it
// refuses before saving (TS applyConfiguration), with a *DocumentError (bad input): auto restarts for
// restart-required changes unless the gateway is foreground, unknown or
// stopping; always restarts; --wait needs a running gateway or a restart.
func RestartDecision(policy string, wait, restartRequired bool, state string, managerAvailable bool) (bool, error) {
	restart := policy == "always"
	running := state == "running" || state == "starting"
	unsafe := state == "foreground" || state == "unknown" || state == "stopping"
	if policy == "auto" && restartRequired {
		if unsafe {
			return false, &DocumentError{Message: "Cannot automatically restart this gateway. Use --restart never to save, then restart its owner explicitly."}
		}
		restart = true
	}
	if restart && (!managerAvailable || unsafe) {
		return false, &DocumentError{Message: "Cannot safely restart this gateway with the service manager."}
	}
	if wait && !restart && (restartRequired || (!running && state != "foreground")) {
		return false, &DocumentError{Message: "--wait requires a running gateway without pending restart changes, or --restart always."}
	}
	return restart, nil
}
