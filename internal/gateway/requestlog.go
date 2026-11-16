package gateway

// uiPollPath is what every visible page polls every few seconds, so it is not
// traced at all. Pages are all one mux pattern, so this goes by path.
const uiPollPath = "/app/strip"

// quietRoute: successful probes and page polls are not request-logged.
func quietRoute(label, path string) bool {
	return label == "/health" || label == "/health/ready" || path == uiPollPath
}
