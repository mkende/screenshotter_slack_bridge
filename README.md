# Screenshotter Slack bridge

Renders screenshotter links posted in Slack as inline image previews, over
Socket Mode, so the screenshotter server needs no public exposure.

- Container image: [ghcr.io/mkende/screenshotter-slack-bridge](https://github.com/users/mkende/packages/container/package/screenshotter-slack-bridge)
- Documentation: [screenshotter.org/slack.html](https://www.screenshotter.org/slack.html)
  (Slack app setup, configuration, deployment); developer notes in
  [`docs/slack.md`](../docs/slack.md)
- Build: `go build -o screenshotter-slack-bridge ./cmd/screenshotter-slack-bridge`
  (or `just build`); no CGo needed
- Config template: [`config.example.toml`](config.example.toml)
