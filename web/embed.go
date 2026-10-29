// Package web embeds the self-contained WMS browser application in releases.
package web

import "embed"

// Assets contains application resources without Node.js or a build service.
//
//go:embed index.html style.css src/*.js vendor/*.js config.example.json
var Assets embed.FS
