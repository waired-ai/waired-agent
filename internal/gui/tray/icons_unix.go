//go:build linux || darwin

package tray

import _ "embed"

//go:embed icons/waired-connected.png
var iconConnected []byte

//go:embed icons/waired-disconnected.png
var iconDisconnected []byte

//go:embed icons/waired-error.png
var iconErrorIcon []byte

//go:embed icons/waired-degraded.png
var iconDegraded []byte
