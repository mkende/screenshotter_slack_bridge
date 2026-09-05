package bridge

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"

	"github.com/mkende/screenshotter_slack_bridge/internal/config"
)

// fakeAPI records calls to the Slack API for assertions.
type fakeAPI struct {
	mu         sync.Mutex
	uploads    []slack.UploadFileV2Parameters
	unfurls    int
	unfurled   map[string]slack.Attachment
	nextFileID string
	unfurlErr  error
	uploadErr  error
	shareErr   error

	uploadHook func() // called inside UploadFileV2Context, outside the lock

	users        map[string]*slack.User
	userErr      error
	groupMembers map[string][]string
	groupErr     error
}

func (f *fakeAPI) UploadFileV2Context(_ context.Context, p slack.UploadFileV2Parameters) (*slack.FileSummary, error) {
	f.mu.Lock()
	f.uploads = append(f.uploads, p)
	hook := f.uploadHook
	uerr := f.uploadErr
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
	if uerr != nil {
		return nil, uerr
	}
	return &slack.FileSummary{ID: f.nextFileID}, nil
}

// publicImageURL is the directly-loadable image URL the fake yields for an
// uploaded file ID, matching publicImageURLFromFile's url_private + pub_secret.
func fakePublicImageURL(fileID string) string {
	return "https://files.slack.test/files-pri/T1-" + fileID + "/" + fileID + ".png?pub_secret=s3cr3t"
}

func (f *fakeAPI) ShareFilePublicURLContext(_ context.Context, fileID string) (*slack.File, []slack.Comment, *slack.Paging, error) {
	if f.shareErr != nil {
		return nil, nil, nil, f.shareErr
	}
	return &slack.File{
		ID:              fileID,
		URLPrivate:      "https://files.slack.test/files-pri/T1-" + fileID + "/" + fileID + ".png",
		PermalinkPublic: "https://slack-files.test/T1-" + fileID + "-s3cr3t",
	}, nil, nil, nil
}

func (f *fakeAPI) UnfurlMessageContext(_ context.Context, _, _ string, unfurls map[string]slack.Attachment, _ ...slack.MsgOption) (string, string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.unfurls++
	// Accumulate across calls, mirroring Slack's per-URL unfurl behavior.
	if f.unfurled == nil {
		f.unfurled = map[string]slack.Attachment{}
	}
	for k, v := range unfurls {
		f.unfurled[k] = v
	}
	return "", "", "", f.unfurlErr
}

func (f *fakeAPI) GetUserInfoContext(_ context.Context, user string) (*slack.User, error) {
	if f.userErr != nil {
		return nil, f.userErr
	}
	u, ok := f.users[user]
	if !ok {
		return &slack.User{ID: user}, nil
	}
	return u, nil
}

func (f *fakeAPI) GetUserGroupMembersContext(_ context.Context, group string) ([]string, error) {
	if f.groupErr != nil {
		return nil, f.groupErr
	}
	return f.groupMembers[group], nil
}

func (f *fakeAPI) uploadCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.uploads)
}

func testPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	img.Set(0, 0, color.RGBA{R: 255, A: 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
}

func pngServer(t *testing.T, data []byte) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Write(data)
	}))
}

// pngServerWithMeta serves a PNG and sets the screenshot metadata headers
// (percent-encoded, as the real server does); empty title/source are omitted. It
// counts how many requests it received.
func pngServerWithMeta(t *testing.T, data []byte, title, source string) (*httptest.Server, *int32) {
	t.Helper()
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		if title != "" {
			w.Header().Set("X-Screenshot-Title", url.QueryEscape(title))
		}
		if source != "" {
			w.Header().Set("X-Screenshot-Source-Url", url.QueryEscape(source))
		}
		w.Header().Set("Content-Type", "image/png")
		w.Write(data)
	}))
	return srv, &hits
}

// unfurlBlocks returns the blocks of the unfurl recorded for link.
func unfurlBlocks(t *testing.T, api *fakeAPI, link string) []slack.Block {
	t.Helper()
	att, ok := api.unfurled[link]
	if !ok {
		t.Fatalf("no unfurl recorded for %q; got %v", link, api.unfurled)
	}
	return att.Blocks.BlockSet
}

// buttonURLs returns the url buttons in an actions block as label->URL, or nil
// if there is no actions block.
func buttonURLs(blocks []slack.Block) map[string]string {
	for _, blk := range blocks {
		ab, ok := blk.(*slack.ActionBlock)
		if !ok || ab.Elements == nil {
			continue
		}
		out := map[string]string{}
		for _, el := range ab.Elements.ElementSet {
			if btn, ok := el.(*slack.ButtonBlockElement); ok {
				out[btn.Text.Text] = btn.URL
			}
		}
		return out
	}
	return nil
}

func newTestBridge(t *testing.T, api slackAPI, cfg *config.Config) *Bridge {
	t.Helper()
	return New(cfg, api, log.New(io.Discard, "", 0))
}

func baseConfig(baseURL string) *config.Config {
	return &config.Config{
		ScreenshotterBaseURL: baseURL,
		UnfurlDomains:        []string{"screen.corp.example", "screen"},
		MaxDimension:         0,
		RequestTimeout:       config.TOMLDuration{Duration: 5 * time.Second},
		MaxConcurrency:       50,
		MaxImageWorkers:      4,
		MaxLinksPerMessage:   4,
	}
}

func TestExtractID(t *testing.T) {
	b := newTestBridge(t, &fakeAPI{}, baseConfig("http://internal:8080"))
	cases := []struct {
		url    string
		wantID string
		wantOK bool
	}{
		{"https://screen.corp.example/abcdef", "abcdef", true},
		{"https://screen.corp.example/abcdef.png", "abcdef", true},
		{"https://screen.corp.example/abcdef.PNG", "abcdef", true}, // uppercase extension
		{"https://screen.corp.example/AbC123", "AbC123", true},
		{"https://screen.corp.example/abcdef/", "abcdef", true},    // trailing slash tolerated
		{"https://screen.corp.example/ab", "ab", true},             // no minimum length anymore
		{"http://screen/abcdef", "abcdef", true},                   // bare host, no .png
		{"https://other.example/abcdef", "", false},                // wrong domain
		{"https://screen.corp.example/upload", "", false},          // reserved endpoint
		{"https://screen.corp.example/Upload", "", false},          // reserved, case-insensitive
		{"https://screen.corp.example/admin", "", false},           // reserved endpoint
		{"https://screen.corp.example/abcdef/annotate", "", false}, // extra path segment
		{"https://screen.corp.example/abc-def", "", false},         // non-alphanumeric
		{"https://screen.corp.example/", "", false},                // no id
	}
	for _, c := range cases {
		got, ok := b.extractID(slackevents.SharedLinks{URL: c.url, Domain: hostOf(t, c.url)})
		if ok != c.wantOK || got != c.wantID {
			t.Errorf("extractID(%q) = (%q,%v), want (%q,%v)", c.url, got, ok, c.wantID, c.wantOK)
		}
	}
}

func TestExtractIDHonorsExtraExcludedPaths(t *testing.T) {
	cfg := baseConfig("http://internal:8080")
	cfg.ExcludedPaths = []string{"health"}
	b := newTestBridge(t, &fakeAPI{}, cfg)
	if _, ok := b.extractID(slackevents.SharedLinks{URL: "https://screen.corp.example/health"}); ok {
		t.Error("expected configured excluded path to be rejected")
	}
}

func hostOf(t *testing.T, raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u.Hostname()
}

// mustParseHost returns the host:port of raw (the canonical-button label form).
func mustParseHost(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u.Host
}

func linkEvent() *slackevents.LinkSharedEvent {
	return &slackevents.LinkSharedEvent{
		User:             "U1",
		Channel:          "C1",
		MessageTimeStamp: "1700000000.000100",
		Links:            []slackevents.SharedLinks{{Domain: "screen.corp.example", URL: "https://screen.corp.example/abcdef"}},
	}
}

func TestRenderInlineUnfurl(t *testing.T) {
	srv := pngServer(t, testPNG(t, 10, 10))
	defer srv.Close()

	api := &fakeAPI{nextFileID: "F123"}
	b := newTestBridge(t, api, baseConfig(srv.URL))
	b.HandleLinkShared(context.Background(), "T1", linkEvent())

	if len(api.uploads) != 1 {
		t.Fatalf("expected 1 upload, got %d", len(api.uploads))
	}
	if api.uploads[0].Channel != "" {
		t.Errorf("inline-unfurl upload should be unshared, got channel %q", api.uploads[0].Channel)
	}
	if api.unfurls != 1 {
		t.Errorf("expected 1 unfurl, got %d", api.unfurls)
	}
}

func TestUnfurlWithoutMetadataHasNoTitleAndOnlyCanonicalButton(t *testing.T) {
	// No metadata headers: the image has no caption and no source button, but the
	// "open on <host>" button is always present (derived from the shared link).
	srv := pngServer(t, testPNG(t, 10, 10))
	defer srv.Close()

	api := &fakeAPI{nextFileID: "F123"}
	b := newTestBridge(t, api, baseConfig(srv.URL))
	b.HandleLinkShared(context.Background(), "T1", linkEvent())

	blocks := unfurlBlocks(t, api, "https://screen.corp.example/abcdef")
	img, ok := blocks[0].(*slack.ImageBlock)
	if !ok {
		t.Fatalf("first block should be an image block, got %T", blocks[0])
	}
	if img.ImageURL != fakePublicImageURL("F123") {
		t.Errorf("image URL = %q, want %q", img.ImageURL, fakePublicImageURL("F123"))
	}
	if img.Title != nil {
		t.Errorf("image should have no title, got %q", img.Title.Text)
	}
	btns := buttonURLs(blocks)
	canon := "open on " + mustParseHost(t, srv.URL)
	if len(btns) != 1 || btns[canon] != srv.URL+"/abcdef" {
		t.Errorf("expected only the canonical (base_url) button, got %v", btns)
	}
}

func TestPublicURLModeUsesBaseURLNotLink(t *testing.T) {
	// In public_url mode the image, metadata, and button all come from base_url —
	// not the posted link's host, which only supplies the ID and the unfurl key.
	// base_url here is the test server; the shared link is a different host.
	srv, hits := pngServerWithMeta(t, testPNG(t, 10, 10), "Login & checkout", "https://app.example/checkout")
	defer srv.Close()

	api := &fakeAPI{nextFileID: "F123"}
	cfg := baseConfig(srv.URL) // base_url = the test server
	cfg.ImageMode = config.ImageModePublicURL
	cfg.UnfurlDomains = []string{"screen.corp.example"}
	b := newTestBridge(t, api, cfg)

	link := "https://screen.corp.example/abcdef" // a host different from base_url
	ev := linkEvent()
	ev.Links = []slackevents.SharedLinks{{Domain: "screen.corp.example", URL: link}}
	b.HandleLinkShared(context.Background(), "T1", ev)

	if api.uploadCount() != 0 {
		t.Errorf("public_url mode should not upload, got %d uploads", api.uploadCount())
	}
	if atomic.LoadInt32(hits) == 0 {
		t.Error("public_url mode should fetch the metadata headers from base_url")
	}
	blocks := unfurlBlocks(t, api, link) // unfurl is keyed by the posted link

	img, ok := blocks[0].(*slack.ImageBlock)
	if !ok {
		t.Fatalf("first block should be the image, got %T", blocks[0])
	}
	if want := srv.URL + "/abcdef.png"; img.ImageURL != want {
		t.Errorf("image URL = %q, want base_url image %q", img.ImageURL, want)
	}
	if img.Title == nil || img.Title.Text != "Login & checkout" {
		t.Errorf("image title = %v, want decoded header %q", img.Title, "Login & checkout")
	}
	btns := buttonURLs(blocks)
	if got := btns["open on "+mustParseHost(t, srv.URL)]; got != srv.URL+"/abcdef" {
		t.Errorf("canonical button URL = %q, want base_url page %q", got, srv.URL+"/abcdef")
	}
	if got := btns["open the source url"]; got != "https://app.example/checkout" {
		t.Errorf("source button URL = %q, want %q", got, "https://app.example/checkout")
	}
}

func TestUploadModeAddsTitleAndCanonicalButton(t *testing.T) {
	// Upload mode reads the same metadata from the PNG fetch. With a title header
	// but no source header, the unfurl gets a captioned image and a single
	// "open on <host>" button derived from base_url.
	srv, _ := pngServerWithMeta(t, testPNG(t, 10, 10), "Homepage", "")
	defer srv.Close()

	api := &fakeAPI{nextFileID: "F123"}
	b := newTestBridge(t, api, baseConfig(srv.URL))
	b.HandleLinkShared(context.Background(), "T1", linkEvent())

	blocks := unfurlBlocks(t, api, "https://screen.corp.example/abcdef")
	img := blocks[0].(*slack.ImageBlock)
	if img.Title == nil || img.Title.Text != "Homepage" {
		t.Errorf("image title = %v, want %q", img.Title, "Homepage")
	}
	btns := buttonURLs(blocks)
	if len(btns) != 1 {
		t.Fatalf("expected only the canonical button, got %v", btns)
	}
	if got := btns["open on "+mustParseHost(t, srv.URL)]; got != srv.URL+"/abcdef" {
		t.Errorf("canonical button URL = %q, want base_url page %q", got, srv.URL+"/abcdef")
	}
}

func TestUploadModeUploadsUnsharedAndSharesPublicly(t *testing.T) {
	srv := pngServer(t, testPNG(t, 10, 10))
	defer srv.Close()

	api := &fakeAPI{nextFileID: "F123"}
	b := newTestBridge(t, api, baseConfig(srv.URL))
	b.HandleLinkShared(context.Background(), "T1", linkEvent())

	if len(api.uploads) != 1 {
		t.Fatalf("expected 1 unshared upload, got %d", len(api.uploads))
	}
	if api.uploads[0].Channel != "" {
		t.Errorf("upload should be unshared (no channel), got %q", api.uploads[0].Channel)
	}
	if api.unfurls != 1 {
		t.Errorf("expected 1 unfurl, got %d", api.unfurls)
	}
}

func TestUnfurlFailureFallsBackToThreadedFilePost(t *testing.T) {
	srv := pngServer(t, testPNG(t, 10, 10))
	defer srv.Close()

	// When the in-place unfurl fails, the bridge posts the screenshot as a file
	// in the message's thread.
	api := &fakeAPI{nextFileID: "F123", unfurlErr: io.ErrUnexpectedEOF}
	b := newTestBridge(t, api, baseConfig(srv.URL))
	b.HandleLinkShared(context.Background(), "T1", linkEvent())

	// 1: the unshared upload for the (failed) unfurl; 2: the threaded file post.
	if len(api.uploads) != 2 {
		t.Fatalf("expected the unfurl upload plus a threaded fallback upload (2), got %d", len(api.uploads))
	}
	fallback := api.uploads[1]
	if fallback.Channel != "C1" || fallback.ThreadTimestamp != "1700000000.000100" {
		t.Errorf("fallback should post into C1's thread, got channel %q thread %q", fallback.Channel, fallback.ThreadTimestamp)
	}
}

func TestNoneModePostsThreadedFileWithoutUnfurling(t *testing.T) {
	srv := pngServer(t, testPNG(t, 10, 10))
	defer srv.Close()

	api := &fakeAPI{nextFileID: "F123"}
	cfg := baseConfig(srv.URL)
	cfg.ImageMode = config.ImageModeNone
	b := newTestBridge(t, api, cfg)
	b.HandleLinkShared(context.Background(), "T1", linkEvent())

	if api.unfurls != 0 {
		t.Errorf("none mode should not unfurl, got %d unfurls", api.unfurls)
	}
	if len(api.uploads) != 1 {
		t.Fatalf("expected exactly the threaded file upload (1), got %d", len(api.uploads))
	}
	up := api.uploads[0]
	if up.Channel != "C1" || up.ThreadTimestamp != "1700000000.000100" {
		t.Errorf("none mode should post into C1's thread, got channel %q thread %q", up.Channel, up.ThreadTimestamp)
	}
}

func TestRenderSkipsMissingImage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()

	api := &fakeAPI{nextFileID: "F123"}
	b := newTestBridge(t, api, baseConfig(srv.URL))
	ev := linkEvent()
	ev.Links[0].URL = "https://screen.corp.example/missing"
	b.HandleLinkShared(context.Background(), "T1", ev)

	if api.uploadCount() != 0 || api.unfurls != 0 {
		t.Errorf("expected no Slack calls for missing image, got %d uploads %d unfurls", api.uploadCount(), api.unfurls)
	}
}

func TestRenderRejectsNonImageContentType(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte("<html>not an image</html>"))
	}))
	defer srv.Close()

	api := &fakeAPI{nextFileID: "F123"}
	b := newTestBridge(t, api, baseConfig(srv.URL))
	b.HandleLinkShared(context.Background(), "T1", linkEvent())

	if api.uploadCount() != 0 {
		t.Errorf("expected no upload for non-image content-type, got %d", api.uploadCount())
	}
}

func TestRenderDoesNotFollowRedirects(t *testing.T) {
	var hitTarget bool
	mux := http.NewServeMux()
	mux.HandleFunc("/abcdef.png", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/elsewhere.png", http.StatusFound)
	})
	mux.HandleFunc("/elsewhere.png", func(w http.ResponseWriter, r *http.Request) {
		hitTarget = true
		w.Header().Set("Content-Type", "image/png")
		w.Write(testPNG(t, 10, 10))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	api := &fakeAPI{nextFileID: "F123"}
	b := newTestBridge(t, api, baseConfig(srv.URL))
	b.HandleLinkShared(context.Background(), "T1", linkEvent())

	if hitTarget {
		t.Error("redirect was followed; the bridge should refuse redirects")
	}
	if api.uploadCount() != 0 {
		t.Errorf("expected no upload after a refused redirect, got %d", api.uploadCount())
	}
}

func TestHandleLinkSharedSkipsComposePreview(t *testing.T) {
	api := &fakeAPI{}
	b := newTestBridge(t, api, baseConfig("http://unused"))
	ev := linkEvent()
	ev.MessageTimeStamp = "a1b2c3d4-e5f6-7890-abcd-ef1234567890" // UUID: compose area
	b.HandleLinkShared(context.Background(), "T1", ev)
	if api.uploadCount() != 0 || api.unfurls != 0 {
		t.Errorf("compose-area preview should be skipped, got %d uploads %d unfurls", api.uploadCount(), api.unfurls)
	}
}

func TestHandleLinkSharedCapsLinksPerMessage(t *testing.T) {
	srv := pngServer(t, testPNG(t, 10, 10))
	defer srv.Close()

	api := &fakeAPI{nextFileID: "F123"}
	cfg := baseConfig(srv.URL)
	cfg.MaxLinksPerMessage = 2
	b := newTestBridge(t, api, cfg)

	ev := linkEvent()
	ev.Links = nil
	for _, id := range []string{"aaaa", "bbbb", "cccc", "dddd"} {
		ev.Links = append(ev.Links, slackevents.SharedLinks{
			Domain: "screen.corp.example",
			URL:    "https://screen.corp.example/" + id,
		})
	}
	b.HandleLinkShared(context.Background(), "T1", ev)

	// Two rendered links (cap=2): two uploads and two per-link unfurl calls.
	if api.uploadCount() != 2 {
		t.Errorf("expected 2 uploads (cap=2), got %d", api.uploadCount())
	}
	if api.unfurls != 2 {
		t.Errorf("expected 2 per-link unfurl calls, got %d", api.unfurls)
	}
	if len(api.unfurled) != 2 {
		t.Errorf("expected 2 links unfurled, got %d: %v", len(api.unfurled), api.unfurled)
	}
}

func TestHandleLinkSharedUnfurlsEachLink(t *testing.T) {
	srv := pngServer(t, testPNG(t, 10, 10))
	defer srv.Close()

	api := &fakeAPI{nextFileID: "F123"}
	b := newTestBridge(t, api, baseConfig(srv.URL))

	ev := linkEvent()
	ev.Links = []slackevents.SharedLinks{
		{Domain: "screen.corp.example", URL: "https://screen.corp.example/aaaa"},
		{Domain: "screen.corp.example", URL: "https://screen.corp.example/bbbb"},
	}
	b.HandleLinkShared(context.Background(), "T1", ev)

	// Each link is unfurled by its own chat.unfurl call; Slack accumulates them
	// per URL, so both end up unfurled.
	if api.unfurls != 2 {
		t.Fatalf("expected one unfurl call per link (2), got %d", api.unfurls)
	}
	for _, link := range []string{"https://screen.corp.example/aaaa", "https://screen.corp.example/bbbb"} {
		if _, ok := api.unfurled[link]; !ok {
			t.Errorf("unfurl is missing link %q; got %v", link, api.unfurled)
		}
	}
}

func TestHandleLinkSharedDropsAtCapacity(t *testing.T) {
	srv := pngServer(t, testPNG(t, 10, 10))
	defer srv.Close()

	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	api := &fakeAPI{nextFileID: "F123", uploadHook: func() {
		once.Do(func() { close(started) })
		<-release
	}}
	cfg := baseConfig(srv.URL)
	cfg.MaxConcurrency = 1
	b := newTestBridge(t, api, cfg)

	// First event occupies the single admission slot and blocks in upload.
	done := make(chan struct{})
	go func() {
		b.HandleLinkShared(context.Background(), "T1", linkEvent())
		close(done)
	}()
	<-started

	// Second event arrives while at capacity: it must be dropped immediately
	// without performing any Slack work.
	b.HandleLinkShared(context.Background(), "T1", linkEvent())

	close(release)
	<-done

	if api.uploadCount() != 1 {
		t.Errorf("expected only the first event to upload (1), got %d; the second should have been dropped", api.uploadCount())
	}
}

func TestAuthzBlocksExternalUsers(t *testing.T) {
	srv := pngServer(t, testPNG(t, 10, 10))
	defer srv.Close()

	api := &fakeAPI{
		nextFileID: "F123",
		users: map[string]*slack.User{
			"Uguest":  {ID: "Uguest", TeamID: "T1", IsRestricted: true},
			"Uextern": {ID: "Uextern", TeamID: "T2"},
			"Uok":     {ID: "Uok", TeamID: "T1"},
		},
	}
	cfg := baseConfig(srv.URL)
	cfg.BlockExternalUsers = true
	b := newTestBridge(t, api, cfg)

	for _, tc := range []struct {
		user        string
		wantUploads int
	}{
		{"Uguest", 0},
		{"Uextern", 0},
		{"Uok", 1},
	} {
		api.uploads = nil
		ev := linkEvent()
		ev.User = tc.user
		b.HandleLinkShared(context.Background(), "T1", ev)
		if api.uploadCount() != tc.wantUploads {
			t.Errorf("user %q: expected %d uploads, got %d", tc.user, tc.wantUploads, api.uploadCount())
		}
	}
}

func TestAuthzFailsClosedOnEmptyTeamID(t *testing.T) {
	srv := pngServer(t, testPNG(t, 10, 10))
	defer srv.Close()

	// A user who would be internal in workspace T1, but the event carries no
	// team ID, so the bridge cannot confirm membership and must deny.
	api := &fakeAPI{
		nextFileID: "F123",
		users:      map[string]*slack.User{"Uok": {ID: "Uok", TeamID: "T1"}},
	}
	cfg := baseConfig(srv.URL)
	cfg.BlockExternalUsers = true
	b := newTestBridge(t, api, cfg)

	ev := linkEvent()
	ev.User = "Uok"
	b.HandleLinkShared(context.Background(), "", ev) // empty teamID
	if api.uploadCount() != 0 {
		t.Errorf("expected fail-closed (no uploads) when team ID is empty, got %d", api.uploadCount())
	}
}

func TestAuthzRestrictsToAllowedGroups(t *testing.T) {
	srv := pngServer(t, testPNG(t, 10, 10))
	defer srv.Close()

	api := &fakeAPI{
		nextFileID:   "F123",
		groupMembers: map[string][]string{"S1": {"Umember"}},
	}
	cfg := baseConfig(srv.URL)
	cfg.AllowedUserGroups = []string{"S1"}
	b := newTestBridge(t, api, cfg)

	for _, tc := range []struct {
		user        string
		wantUploads int
	}{
		{"Umember", 1},
		{"Ustranger", 0},
	} {
		api.uploads = nil
		ev := linkEvent()
		ev.User = tc.user
		b.HandleLinkShared(context.Background(), "T1", ev)
		if api.uploadCount() != tc.wantUploads {
			t.Errorf("user %q: expected %d uploads, got %d", tc.user, tc.wantUploads, api.uploadCount())
		}
	}
}

func TestAuthzFailsClosedOnAPIError(t *testing.T) {
	srv := pngServer(t, testPNG(t, 10, 10))
	defer srv.Close()

	api := &fakeAPI{nextFileID: "F123", userErr: io.ErrUnexpectedEOF}
	cfg := baseConfig(srv.URL)
	cfg.BlockExternalUsers = true
	b := newTestBridge(t, api, cfg)

	b.HandleLinkShared(context.Background(), "T1", linkEvent())
	if api.uploadCount() != 0 {
		t.Errorf("expected no uploads when user lookup fails (fail closed), got %d", api.uploadCount())
	}
}
