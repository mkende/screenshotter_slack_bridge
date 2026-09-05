// Package bridge contains the core logic that turns a Slack link_shared event
// for a screenshotter link into an inline image unfurl (falling back to a
// threaded file upload when an inline unfurl is not possible).
package bridge

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"

	"github.com/mkende/screenshotter_slack_bridge/internal/config"
	"github.com/mkende/screenshotter_slack_bridge/internal/imageproc"
)

// maxImageBytes bounds how much we read from the screenshotter server, as a
// safety net against an unexpectedly huge response.
const maxImageBytes = 64 << 20 // 64 MiB

// errNotFound is returned when the screenshotter server has no such image.
var errNotFound = errors.New("image not found")

// idPattern matches the random alphanumeric image IDs the server generates.
// Keeping IDs strictly alphanumeric also prevents path traversal or injection
// into the fetch URL.
var idPattern = regexp.MustCompile(`^[a-zA-Z0-9]+$`)

// slackTSPattern matches a real Slack message timestamp ("1700000000.000100").
// The compose-area variant of link_shared carries a UUID instead, which cannot
// be unfurled, so we use this to skip those events.
var slackTSPattern = regexp.MustCompile(`^\d+\.\d+$`)

// defaultReservedPaths are first path segments that are known server endpoints
// rather than image IDs, so a link to one must never be fetched as an image.
//
// IMPORTANT: keep this in sync with the screenshotter server's routes. If a new
// top-level route is added to the server (see internal/server/server.go), add
// its first path segment here. Operators can also extend the list via the
// excluded_paths config key without editing this file.
var defaultReservedPaths = []string{
	"upload", "assets", "static", "thumb", "admin", "auth", "favicon.ico",
}

// slackAPI is the subset of the Slack client the bridge uses.
type slackAPI interface {
	UploadFileV2Context(ctx context.Context, params slack.UploadFileV2Parameters) (*slack.FileSummary, error)
	ShareFilePublicURLContext(ctx context.Context, fileID string) (*slack.File, []slack.Comment, *slack.Paging, error)
	UnfurlMessageContext(ctx context.Context, channelID, timestamp string, unfurls map[string]slack.Attachment, options ...slack.MsgOption) (string, string, string, error)
	GetUserInfoContext(ctx context.Context, user string) (*slack.User, error)
	GetUserGroupMembersContext(ctx context.Context, userGroup string) ([]string, error)
}

// Bridge renders screenshotter links into Slack.
type Bridge struct {
	cfg      *config.Config
	api      slackAPI
	http     *http.Client
	domains  map[string]bool
	reserved map[string]bool
	// admit bounds the number of link_shared events handled concurrently
	// ("the rest": fetch + Slack calls). It is acquired non-blocking, so a
	// flood drops events instead of accumulating goroutines.
	admit chan struct{}
	// convert bounds the number of concurrent image decode/resize operations,
	// the memory-heavy step, independently of admit.
	convert chan struct{}
	authz   *authorizer // nil when no user restrictions are configured
	log     *log.Logger
}

// New creates a Bridge. api is typically a *slack.Client.
func New(cfg *config.Config, api slackAPI, logger *log.Logger) *Bridge {
	domains := make(map[string]bool, len(cfg.UnfurlDomains))
	for _, d := range cfg.UnfurlDomains {
		domains[d] = true
	}
	reserved := make(map[string]bool, len(defaultReservedPaths)+len(cfg.ExcludedPaths))
	for _, p := range defaultReservedPaths {
		reserved[p] = true
	}
	for _, p := range cfg.ExcludedPaths {
		reserved[p] = true
	}

	var az *authorizer
	if cfg.BlockExternalUsers || len(cfg.AllowedUserGroups) > 0 {
		az = newAuthorizer(api, cfg, logger)
	}

	return &Bridge{
		cfg:      cfg,
		api:      api,
		http:     newHTTPClient(cfg.RequestTimeout.Duration),
		domains:  domains,
		reserved: reserved,
		admit:    make(chan struct{}, cfg.MaxConcurrency),
		convert:  make(chan struct{}, cfg.MaxImageWorkers),
		authz:    az,
		log:      logger,
	}
}

// newHTTPClient builds the client used to fetch images. It refuses to follow
// redirects: the screenshotter server serves PNGs directly, and following a
// redirect could send the request to an unintended host.
func newHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// HandleLinkShared processes every recognised link in a link_shared event.
// teamID is the workspace the event was delivered to, used for external-user
// checks.
func (b *Bridge) HandleLinkShared(ctx context.Context, teamID string, ev *slackevents.LinkSharedEvent) {
	defer func() {
		if r := recover(); r != nil {
			b.log.Printf("recovered from panic handling link_shared: %v", r)
		}
	}()

	if !slackTSPattern.MatchString(ev.MessageTimeStamp) {
		// Compose-area preview (source "composer"): the message_ts is a
		// userID-uuid-hash unfurl_id rather than a real timestamp, there is no
		// posted message to attach to, and chat.unfurl by channel+ts cannot act
		// on it. Slack fires one per keystroke while a link is being typed, so
		// log only under debug to avoid flooding normal operation.
		if b.cfg.Debug {
			b.log.Printf("ignoring composer-area link_shared (non-message ts %q)", ev.MessageTimeStamp)
		}
		return
	}

	// Admission control: bound concurrent handlers. A non-blocking acquire means
	// that under a flood we drop events instead of letting goroutines (and their
	// downstream Slack work) accumulate without limit.
	select {
	case b.admit <- struct{}{}:
		defer func() { <-b.admit }()
	default:
		b.log.Printf("at capacity (%d concurrent), dropping link_shared for message %q",
			cap(b.admit), ev.MessageTimeStamp)
		return
	}

	if b.authz != nil && !b.authz.Allow(ctx, ev.User, teamID) {
		b.log.Printf("skipping link_shared from unauthorized user %q", ev.User)
		return
	}

	// Unfurl each link in its own chat.unfurl call. Slack accumulates unfurls per
	// URL across calls, so independent calls let every good link render even if
	// another link in the same message fails.
	rendered := 0
	for _, link := range ev.Links {
		id, ok := b.extractID(link)
		if !ok {
			continue
		}
		if rendered >= b.cfg.MaxLinksPerMessage {
			b.log.Printf("link cap (%d) reached for message %q; skipping remaining links",
				b.cfg.MaxLinksPerMessage, ev.MessageTimeStamp)
			break
		}
		rendered++
		if err := b.render(ctx, ev, link.URL, id); err != nil {
			if errors.Is(err, errNotFound) {
				b.log.Printf("image %q not found, skipping", id)
				continue
			}
			b.log.Printf("rendering %q failed: %v", id, err)
		}
	}
}

// render resolves a public image URL for the link's id — per the configured
// image mode — and unfurls the link in place with the screenshot image, its
// title, and the link buttons. The memory-heavy image conversion (upload mode
// only) is bounded by resize.
func (b *Bridge) render(ctx context.Context, ev *slackevents.LinkSharedEvent, link, id string) error {
	alt := "Screenshot " + id

	// image_mode "none" never unfurls in place — it only posts the threaded file
	// reply.
	if b.cfg.ImageMode == config.ImageModeNone {
		return b.postImageReply(ctx, ev, id, alt)
	}

	// Try the in-place unfurl; on any failure other than a vanished image, fall
	// back to posting the screenshot as a threaded file reply.
	if err := b.unfurlInPlace(ctx, ev, link, id, alt); err != nil {
		if errors.Is(err, errNotFound) {
			return err
		}
		b.log.Printf("in-place unfurl for %q failed (%v); falling back to a threaded file post", id, err)
		return b.postImageReply(ctx, ev, id, alt)
	}
	return nil
}

// unfurlInPlace renders the link in place with the screenshot image, per the
// configured image mode. The image, metadata, and buttons all come from
// screenshotter_base_url, the canonical server URL — not the posted link's host.
// The link only carries the ID, so any registered domain (even a bare
// single-label host) resolves to the same Slack-loadable image.
func (b *Bridge) unfurlInPlace(ctx context.Context, ev *slackevents.LinkSharedEvent, link, id, alt string) error {
	imageURL := b.imageFileURL(id)

	var meta imageMeta
	var err error
	if b.cfg.ImageMode == config.ImageModePublicURL {
		// Slack loads the bytes from imageURL itself; we only fetch the metadata
		// headers. A fetch failure (other than a vanished image) is non-fatal —
		// we still unfurl the image, just without the title/source extras.
		meta, err = b.fetchMeta(ctx, imageURL)
		if errors.Is(err, errNotFound) {
			return err
		} else if err != nil {
			b.log.Printf("metadata fetch for %q failed (%v); unfurling without title/source", id, err)
		}
	} else {
		// Upload mode: fetch from base_url, upload to Slack, and point the image
		// block at the Slack-hosted public URL instead.
		imageURL, meta, err = b.uploadPublicImage(ctx, id, alt)
		if err != nil {
			return err
		}
	}

	blocks := previewBlocks(b.cfg.ScreenshotterBaseURL, id, imageURL, alt, meta)
	unfurls := map[string]slack.Attachment{link: {Blocks: slack.Blocks{BlockSet: blocks}}}
	if uerr := b.unfurl(ctx, ev.Channel, ev.MessageTimeStamp, unfurls); uerr != nil {
		return fmt.Errorf("unfurl: %s", slackErr(uerr))
	}
	return nil
}

// postImageReply posts the screenshot as a file in the message's thread. The
// file stays workspace-private (it is never made public), but the bot must be a
// member of the channel. Used as image_mode "none" and as the fallback when an
// in-place unfurl fails.
func (b *Bridge) postImageReply(ctx context.Context, ev *slackevents.LinkSharedEvent, id, alt string) error {
	data, _, err := b.fetchPNG(ctx, id)
	if err != nil {
		return err // may be errNotFound; let the caller classify it
	}
	data, err = b.resize(ctx, data)
	if err != nil {
		return fmt.Errorf("resize: %w", err)
	}

	threadTS := ev.ThreadTimeStamp
	if threadTS == "" {
		threadTS = ev.MessageTimeStamp
	}
	filename := id + ".png"
	if _, err := b.uploadFile(ctx, slack.UploadFileV2Parameters{
		Filename:        filename,
		Title:           filename,
		FileSize:        len(data),
		Reader:          bytes.NewReader(data),
		AltTxt:          alt,
		Channel:         ev.Channel,
		ThreadTimestamp: threadTS,
	}); err != nil {
		return fmt.Errorf("threaded file post: %s", slackErr(err))
	}
	return nil
}

// imageFileURL is the canonical PNG URL on the screenshotter server: the address
// the bridge fetches in upload mode and that Slack loads directly in public_url
// mode. base_url is validated and trailing-slash-trimmed by config.
func (b *Bridge) imageFileURL(id string) string {
	return b.cfg.ScreenshotterBaseURL + "/" + id + ".png"
}

// uploadPublicImage fetches the screenshot over the (possibly private) network,
// uploads it to Slack, makes it public, and returns a direct image URL Slack can
// load in the unfurl (image mode "upload") plus the screenshot metadata.
func (b *Bridge) uploadPublicImage(ctx context.Context, id, alt string) (string, imageMeta, error) {
	data, meta, err := b.fetchPNG(ctx, id)
	if err != nil {
		return "", imageMeta{}, err // may be errNotFound; let the caller classify it
	}
	data, err = b.resize(ctx, data)
	if err != nil {
		return "", imageMeta{}, fmt.Errorf("resize: %w", err)
	}

	filename := id + ".png"
	summary, err := b.uploadFile(ctx, slack.UploadFileV2Parameters{
		Filename: filename,
		Title:    filename,
		FileSize: len(data),
		Reader:   bytes.NewReader(data),
		AltTxt:   alt,
	})
	if err != nil {
		return "", imageMeta{}, fmt.Errorf("upload: %s", slackErr(err))
	}
	if summary == nil || summary.ID == "" {
		return "", imageMeta{}, errors.New("upload returned no file ID")
	}

	imageURL, err := b.publicFileURL(ctx, summary.ID)
	if err != nil {
		return "", imageMeta{}, fmt.Errorf("make file public: %s", slackErr(err))
	}
	return imageURL, meta, nil
}

// slackErr renders a Slack API error with the detail Slack attaches in
// response_metadata.messages. The slack-go error's own string is only the short
// code (e.g. "invalid_blocks"); the per-block validation messages, which say
// exactly what was rejected, live in the metadata and are otherwise dropped.
func slackErr(err error) string {
	var se slack.SlackErrorResponse
	if errors.As(err, &se) && len(se.ResponseMetadata.Messages) > 0 {
		return fmt.Sprintf("%s: %s", se.Err, strings.Join(se.ResponseMetadata.Messages, "; "))
	}
	return err.Error()
}

// imageBlock builds an image block that loads the screenshot from a publicly
// reachable URL — the only image source the link-unfurling API supports.
// (slack_file references are rejected in unfurls, so the bytes must be served
// from a URL Slack can fetch: the public server, or a public Slack file URL.)
// A non-empty title is shown as the image's caption.
func imageBlock(imageURL, title, alt string) *slack.ImageBlock {
	var titleObj *slack.TextBlockObject
	if title != "" {
		titleObj = slack.NewTextBlockObject(slack.PlainTextType, title, false, false)
	}
	return slack.NewImageBlock(imageURL, alt, "", titleObj)
}

// previewBlocks builds the unfurl's block kit: the screenshot image (captioned
// with the title when the server provided one) followed by an actions row with
// "open on <host>" (linking to the screenshot's page on the canonical server,
// baseURL) and "open the source url" (only when the server provided a source URL).
func previewBlocks(baseURL, id, imageURL, alt string, meta imageMeta) []slack.Block {
	blocks := []slack.Block{imageBlock(imageURL, meta.title, alt)}
	if actions := actionButtons(baseURL, id, meta.sourceURL); actions != nil {
		blocks = append(blocks, actions)
	}
	return blocks
}

// actionButtons builds the row of link buttons under the image, or nil if there
// is nothing to link to. The "open on <host>" button points at the screenshot's
// page on the canonical server (baseURL), so it works regardless of which
// registered domain the user typed.
func actionButtons(baseURL, id, sourceURL string) *slack.ActionBlock {
	var elems []slack.BlockElement
	if u, err := url.Parse(baseURL); err == nil && u.Host != "" {
		elems = append(elems, urlButton("open_canonical", "open on "+u.Host, baseURL+"/"+id))
	}
	if sourceURL != "" {
		elems = append(elems, urlButton("open_source", "open the source url", sourceURL))
	}
	if len(elems) == 0 {
		return nil
	}
	return slack.NewActionBlock("", elems...)
}

// urlButton builds a link button: clicking it opens linkURL. Slack still sends a
// block_actions payload for url buttons, which the bridge acknowledges and
// ignores (see the Socket Mode loop), so Interactivity must be enabled.
func urlButton(actionID, text, linkURL string) *slack.ButtonBlockElement {
	btn := slack.NewButtonBlockElement(actionID, "", slack.NewTextBlockObject(slack.PlainTextType, text, false, false))
	btn.URL = linkURL
	return btn
}

// resize runs the (optional) image conversion under the convert semaphore, so
// the number of simultaneous decode/resize operations — the memory-heavy step —
// stays bounded independently of the overall admission limit.
func (b *Bridge) resize(ctx context.Context, data []byte) ([]byte, error) {
	select {
	case b.convert <- struct{}{}:
		defer func() { <-b.convert }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return imageproc.MaybeResize(data, b.cfg.MaxDimension)
}

// uploadFile uploads to Slack with a per-request timeout.
func (b *Bridge) uploadFile(ctx context.Context, params slack.UploadFileV2Parameters) (*slack.FileSummary, error) {
	uctx, cancel := context.WithTimeout(ctx, b.cfg.RequestTimeout.Duration)
	defer cancel()
	return b.api.UploadFileV2Context(uctx, params)
}

// publicFileURL makes an uploaded file publicly retrievable and returns a direct
// image URL Slack can load, with a per-request timeout. files.sharedPublicURL
// returns the file's public secret in permalink_public; the loadable image URL
// is url_private with that secret appended as pub_secret.
func (b *Bridge) publicFileURL(ctx context.Context, fileID string) (string, error) {
	fctx, cancel := context.WithTimeout(ctx, b.cfg.RequestTimeout.Duration)
	defer cancel()
	file, _, _, err := b.api.ShareFilePublicURLContext(fctx, fileID)
	if err != nil {
		return "", err
	}
	return publicImageURLFromFile(file)
}

// publicImageURLFromFile derives a directly-loadable image URL from a file that
// has been shared publicly, by appending its pub_secret to the private URL. The
// secret is the last "-"-separated segment of permalink_public.
func publicImageURLFromFile(file *slack.File) (string, error) {
	if file == nil || file.URLPrivate == "" || file.PermalinkPublic == "" {
		return "", errors.New("shared file is missing url_private or permalink_public")
	}
	i := strings.LastIndex(file.PermalinkPublic, "-")
	if i < 0 || i == len(file.PermalinkPublic)-1 {
		return "", fmt.Errorf("unexpected permalink_public format %q", file.PermalinkPublic)
	}
	secret := file.PermalinkPublic[i+1:]
	sep := "?"
	if strings.Contains(file.URLPrivate, "?") {
		sep = "&"
	}
	return file.URLPrivate + sep + "pub_secret=" + secret, nil
}

// unfurl replaces a shared link with an inline preview, with a per-request
// timeout.
func (b *Bridge) unfurl(ctx context.Context, channel, ts string, unfurls map[string]slack.Attachment) error {
	uctx, cancel := context.WithTimeout(ctx, b.cfg.RequestTimeout.Duration)
	defer cancel()
	_, _, _, err := b.api.UnfurlMessageContext(uctx, channel, ts, unfurls)
	return err
}

// imageMeta holds the optional screenshot metadata the server exposes as headers
// on the image response. Empty strings mean the header was absent.
type imageMeta struct {
	title     string // X-Screenshot-Title
	sourceURL string // X-Screenshot-Source-Url
}

// parseImageMeta reads the screenshot metadata headers, percent-decoding them
// (the server percent-encodes the values so arbitrary text is a valid header).
// A missing or malformed value yields an empty string.
func parseImageMeta(h http.Header) imageMeta {
	return imageMeta{
		title:     decodeMetaHeader(h.Get("X-Screenshot-Title")),
		sourceURL: decodeMetaHeader(h.Get("X-Screenshot-Source-Url")),
	}
}

func decodeMetaHeader(v string) string {
	if v == "" {
		return ""
	}
	decoded, err := url.QueryUnescape(v)
	if err != nil {
		return "" // malformed; treat as absent rather than surfacing raw bytes
	}
	return decoded
}

// fetchPNG downloads the full-size image for id from the screenshotter server,
// returning the bytes and the metadata headers (upload mode).
func (b *Bridge) fetchPNG(ctx context.Context, id string) ([]byte, imageMeta, error) {
	imgURL := b.imageFileURL(id)

	fctx, cancel := context.WithTimeout(ctx, b.cfg.RequestTimeout.Duration)
	defer cancel()

	req, err := http.NewRequestWithContext(fctx, http.MethodGet, imgURL, nil)
	if err != nil {
		return nil, imageMeta{}, err
	}
	resp, err := b.http.Do(req)
	if err != nil {
		return nil, imageMeta{}, fmt.Errorf("fetch %s: %w", imgURL, err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return nil, imageMeta{}, errNotFound
	default:
		return nil, imageMeta{}, fmt.Errorf("fetch %s: unexpected status %d", imgURL, resp.StatusCode)
	}

	// Safeguard: only forward responses the server labels as an image.
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(strings.ToLower(ct), "image/") {
		return nil, imageMeta{}, fmt.Errorf("fetch %s: unexpected content-type %q", imgURL, ct)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxImageBytes+1))
	if err != nil {
		return nil, imageMeta{}, fmt.Errorf("read %s: %w", imgURL, err)
	}
	if len(data) > maxImageBytes {
		return nil, imageMeta{}, fmt.Errorf("image %s exceeds %d bytes", id, maxImageBytes)
	}
	return data, parseImageMeta(resp.Header), nil
}

// fetchMeta reads only the screenshot metadata headers from imgURL, without
// downloading the image body (public_url mode, where Slack loads the bytes
// itself). It returns errNotFound for a 404 so a vanished image is skipped.
func (b *Bridge) fetchMeta(ctx context.Context, imgURL string) (imageMeta, error) {
	fctx, cancel := context.WithTimeout(ctx, b.cfg.RequestTimeout.Duration)
	defer cancel()

	req, err := http.NewRequestWithContext(fctx, http.MethodGet, imgURL, nil)
	if err != nil {
		return imageMeta{}, err
	}
	resp, err := b.http.Do(req)
	if err != nil {
		return imageMeta{}, fmt.Errorf("fetch %s: %w", imgURL, err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return imageMeta{}, errNotFound
	default:
		return imageMeta{}, fmt.Errorf("fetch %s: unexpected status %d", imgURL, resp.StatusCode)
	}
	return parseImageMeta(resp.Header), nil
}

// extractID returns the image ID encoded in a shared link, and whether the
// link is one this bridge should handle. It accepts both /{id} and /{id}.png on
// any configured domain, and rejects known server endpoints.
func (b *Bridge) extractID(link slackevents.SharedLinks) (string, bool) {
	u, err := url.Parse(link.URL)
	if err != nil {
		return "", false
	}
	host := strings.ToLower(u.Hostname())
	if !b.domains[host] {
		return "", false
	}
	path := strings.TrimPrefix(u.EscapedPath(), "/")
	path = strings.TrimSuffix(path, "/")
	if strings.HasSuffix(strings.ToLower(path), ".png") {
		path = path[:len(path)-len(".png")]
	}
	if path == "" || strings.Contains(path, "/") {
		return "", false
	}
	if !idPattern.MatchString(path) {
		return "", false
	}
	if b.reserved[strings.ToLower(path)] {
		return "", false
	}
	return path, true
}
