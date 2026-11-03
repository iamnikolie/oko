// Package extension carries the oko Chrome extension (MV3) inside the binary;
// 'oko ext path' writes it out for "Load unpacked".
package extension

import "embed"

// Files is the unpacked extension.
//
//go:embed manifest.json background.js restricted.js protocol.js popup.html popup.css popup.js cdp-policy.json
var Files embed.FS
