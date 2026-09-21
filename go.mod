module github.com/mkende/screenshotter_slack_bridge

go 1.26.0

toolchain go1.27.1

require (
	github.com/BurntSushi/toml v1.6.0
	// Temporary: the unfurl sets hide_color, a field slack-go does not have
	// yet, so this is a fork of github.com/slack-go/slack carrying the patch.
	// Switch back to the upstream module once
	// https://github.com/slack-go/slack/pull/1590 lands.
	//
	// It is required directly rather than through a replace directive because
	// `go install pkg@version` refuses any module whose go.mod carries one,
	// which would leave the published bridge uninstallable.
	github.com/mkende/slack-go v0.0.0-20260912162126-8cc1d2229508
	golang.org/x/image v0.46.0
)

require github.com/gorilla/websocket v1.5.3 // indirect
