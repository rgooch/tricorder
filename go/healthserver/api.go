/*
	Package healthserver registers HTTP handlers for health and readiness checks

	Package healthserver registers HTTP handlers for the /healthz and
	/readiness paths. These handlers are always registered (this is done at
	package init time).

	By default, the /health handler responds with "OK".

	By default, the /readiness handler responds with a "503 Service Unavailable"
	HTTP status and followed by "not ready".
*/
package healthserver

// SetHealthy will make the /healthz HTTP handler respond with "OK".
func SetHealthy() {
	setHealth("")
}

// SetNotHealthy will make the /healthz HTTP handler respond with a
// "503 Service Unavailable" HTTP status followed by the status string.
// Passing "" or "OK" for status will result in a panic.
func SetNotHealthy(status string) {
	if status == "" || status == "OK" {
		panic("OK status not permitted")
	}
	setHealth(status)
}

// SetNotReady will make the /readiness HTTP handler respond with a
// "503 Service Unavailable" HTTP status followed by the status string.
// Passing "" or "OK" for status will result in a panic.
func SetNotReady(status string) {
	if status == "" || status == "OK" {
		panic("OK status not permitted")
	}
	setReady(status)
}

// SetReady will make the /readiness HTTP handler respond with "OK".
func SetReady() {
	setReady("")
}
