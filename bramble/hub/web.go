package hub

import (
	_ "embed"
	"net/http"
)

//go:embed web/index.html
var indexHTML []byte

// handleIndex serves the single-page web UI.
func (h *Hub) handleIndex(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(indexHTML)
}
