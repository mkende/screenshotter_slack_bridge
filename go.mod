module github.com/mkende/screenshotter_slack_bridge

go 1.26

toolchain go1.27.0

require (
	github.com/BurntSushi/toml v1.6.0
	github.com/slack-go/slack v0.29.0
	golang.org/x/image v0.45.0
)

require github.com/gorilla/websocket v1.5.3 // indirect

replace github.com/slack-go/slack => github.com/mkende/slack-go v0.0.0-20260908205621-941a19f30d5c
