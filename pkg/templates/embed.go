// Package templates holds the HTML templates for the web portal
// (§3, §4). The .tpl files in this directory are compiled
// into the binary with go:embed so the GUI works with zero files on disk,
// air-gapped, from a single binary.
//
// Layout convention: base.tpl is the shared document shell (head, nav,
// alert banner); every other .tpl defines a "content" block that base.tpl
// pulls in. pkg/renderer pairs each page with the base at parse time.
package templates

import "embed"

// FS exposes every .tpl file in this directory as an embedded read-only
// filesystem. pkg/renderer parses it once at startup.
//
//go:embed *.tpl
var FS embed.FS
