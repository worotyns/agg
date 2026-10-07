// Package web embeds the admin UI and the browser SDK into the binary.
package web

import (
	"embed"
)

//go:embed ui
var UI embed.FS

//go:embed sdk/agg.js
var SDK []byte

// Index returns the UI entry page.
func Index() []byte {
	b, _ := UI.ReadFile("ui/index.html")
	return b
}
