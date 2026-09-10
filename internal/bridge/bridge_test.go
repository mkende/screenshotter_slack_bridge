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
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"

	"github.com/mkende/screenshotter_slack_bridge/internal/config"
	"github.com/mkende/screenshotter_slack_bridge/internal/imageproc"
)

// remoteAdd is one recorded files.remote.add call, with the preview image read
// out of its reader so assertions can decode it.
type remoteAdd struct {
	params  slack.RemoteFileParameters
	preview []byte
}

// fakeAPI records calls to the Slack API for assertions.
type fakeAPI struct {
	mu       sync.Mutex
	adds     []remoteAdd
	infos    int
	unfurls  int
	unfurled map[string]slack.Attachment

	unfurlErr error
	addErr    error
	infoErr   error
	// firstPollAfter is the number of files.remote.add calls that had completed
	// when files.remote.info was first called.
	firstPollAfter int
	// richAfter is how many files.remote.info calls report the preview as not
	// ready before one reports it ready. 0 means ready on the first call.
	richAfter int

	addHook func() // called inside AddRemoteFileContext, outside the lock

	users        map[string]*slack.User
	userErr      error
	groupMembers map[string][]string
	groupErr     error
}

func (f *fakeAPI) AddRemoteFileContext(_ context.Context, p slack.RemoteFileParameters) (*slack.RemoteFile, error) {
	var preview []byte
	if p.PreviewImageReader != nil {
		var err error
		if preview, err = io.ReadAll(p.PreviewImageReader); err != nil {
			return nil, err
		}
		p.PreviewImageReader = nil // recorded as preview instead
	}
	f.mu.Lock()
	f.adds = append(f.adds, remoteAdd{params: p, preview: preview})
	hook := f.addHook
	aerr := f.addErr
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
	if aerr != nil {
		return nil, aerr
	}
	return &slack.RemoteFile{ID: "F1", ExternalID: p.ExternalID}, nil
}

func (f *fakeAPI) GetRemoteFileInfoContext(_ context.Context, externalID, _ string) (*slack.RemoteFile, error) {
	if f.infoErr != nil {
		return nil, f.infoErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.infos == 0 {
		f.firstPollAfter = len(f.adds)
	}
	f.infos++
	return &slack.RemoteFile{ExternalID: externalID, HasRichPreview: f.infos > f.richAfter}, nil
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

func (f *fakeAPI) GetUserGroupMembersContext(_ context.Context, group string, _ ...slack.GetUserGroupMembersOption) ([]string, error) {
	if f.groupErr != nil {
		return nil, f.groupErr
	}
	return f.groupMembers[group], nil
}

func (f *fakeAPI) addCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.adds)
}

func (f *fakeAPI) infoCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.infos
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

// unfurlFor returns the unfurl attachment recorded for link.
func unfurlFor(t *testing.T, api *fakeAPI, link string) slack.Attachment {
	t.Helper()
	att, ok := api.unfurled[link]
	if !ok {
		t.Fatalf("no unfurl recorded for %q; got %v", link, api.unfurled)
	}
	return att
}

// onlyFileBlock returns the unfurl's single file block, failing if the unfurl
// carries anything else: Slack rejects a file block combined with other blocks.
func onlyFileBlock(t *testing.T, blocks []slack.Block) *slack.FileBlock {
	t.Helper()
	if len(blocks) != 1 {
		t.Fatalf("unfurl should carry exactly one block, got %d: %v", len(blocks), blocks)
	}
	fb, ok := blocks[0].(*slack.FileBlock)
	if !ok {
		t.Fatalf("unfurl block should be a file block, got %T", blocks[0])
	}
	return fb
}

// previewDims decodes the recorded preview image's dimensions.
func previewDims(t *testing.T, data []byte) (int, int) {
	t.Helper()
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decode preview: %v", err)
	}
	return cfg.Width, cfg.Height
}

func newTestBridge(t *testing.T, api slackAPI, cfg *config.Config) *Bridge {
	t.Helper()
	return New(cfg, api, log.New(io.Discard, "", 0))
}

func baseConfig(baseURL string) *config.Config {
	return &config.Config{
		ScreenshotterBaseURL: baseURL,
		UnfurlDomains:        []string{"screen.corp.example", "screen"},
		MaxDimension:         1600,
		// The preview wait is exercised by its own test; elsewhere it would only
		// add polling to every case.
		PreviewWait:        config.TOMLDuration{Duration: 0},
		RequestTimeout:     config.TOMLDuration{Duration: 5 * time.Second},
		MaxConcurrency:     50,
		MaxImageWorkers:    4,
		MaxLinksPerMessage: 4,
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

func linkEvent() *slackevents.LinkSharedEvent {
	return &slackevents.LinkSharedEvent{
		User:             "U1",
		Channel:          "C1",
		MessageTimeStamp: "1700000000.000100",
		Links:            []slackevents.SharedLinks{{Domain: "screen.corp.example", URL: "https://screen.corp.example/abcdef"}},
	}
}

func TestRenderUnfurlsWithASingleFileBlock(t *testing.T) {
	srv := pngServer(t, testPNG(t, 800, 600))
	defer srv.Close()

	api := &fakeAPI{}
	b := newTestBridge(t, api, baseConfig(srv.URL))
	b.HandleLinkShared(context.Background(), "T1", linkEvent())

	if len(api.adds) != 1 {
		t.Fatalf("expected 1 files.remote.add, got %d", len(api.adds))
	}
	if api.unfurls != 1 {
		t.Fatalf("expected 1 unfurl, got %d", api.unfurls)
	}
	att := unfurlFor(t, api, "https://screen.corp.example/abcdef")
	fb := onlyFileBlock(t, att.Blocks.BlockSet)
	// The block must name the file that was just added, not the image ID.
	if want := api.adds[0].params.ExternalID; fb.ExternalID != want || fb.Source != "remote" {
		t.Errorf("file block = (external_id %q, source %q), want (%q, %q)", fb.ExternalID, fb.Source, want, "remote")
	}
	if !att.HideColor {
		t.Error("unfurl should set hide_color, dropping the card's colour bar")
	}
}

func TestRemoteFileLinksToTheBaseURLPageNotTheLinkHost(t *testing.T) {
	// The card's click-through is the screenshot's page on base_url; the posted
	// link's host only supplies the ID and keys the unfurl.
	srv, hits := pngServerWithMeta(t, testPNG(t, 800, 600), "Login & checkout", "https://app.example/checkout")
	defer srv.Close()

	api := &fakeAPI{}
	cfg := baseConfig(srv.URL) // base_url = the test server
	cfg.UnfurlDomains = []string{"screen.corp.example"}
	b := newTestBridge(t, api, cfg)

	link := "https://screen.corp.example/abcdef" // a host different from base_url
	ev := linkEvent()
	ev.Links = []slackevents.SharedLinks{{Domain: "screen.corp.example", URL: link}}
	b.HandleLinkShared(context.Background(), "T1", ev)

	if atomic.LoadInt32(hits) != 1 {
		t.Errorf("expected exactly one fetch from base_url, got %d", atomic.LoadInt32(hits))
	}
	p := api.adds[0].params
	if want := srv.URL + "/abcdef"; p.ExternalURL != want {
		t.Errorf("external URL = %q, want the base_url page %q", p.ExternalURL, want)
	}
	if p.Title != "Login & checkout" {
		t.Errorf("title = %q, want the decoded header %q", p.Title, "Login & checkout")
	}
	if !strings.Contains(p.IndexableFileContents, "https://app.example/checkout") {
		t.Errorf("indexable contents = %q, want it to carry the source URL", p.IndexableFileContents)
	}
	if _, ok := api.unfurled[link]; !ok {
		t.Errorf("unfurl should be keyed by the posted link; got %v", api.unfurled)
	}
}

func TestRemoteFileTitleFallsBackToTheSourceURLThenTheID(t *testing.T) {
	for _, tc := range []struct {
		name, title, source, want string
	}{
		{"title wins", "Checkout page", "https://app.example/checkout", "Checkout page"},
		{"source url when untitled", "", "https://app.example/checkout", "https://app.example/checkout"},
		{"id when neither", "", "", "Screenshot abcdef"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := pngServerWithMeta(t, testPNG(t, 800, 600), tc.title, tc.source)
			defer srv.Close()

			api := &fakeAPI{}
			b := newTestBridge(t, api, baseConfig(srv.URL))
			b.HandleLinkShared(context.Background(), "T1", linkEvent())

			if got := api.adds[0].params.Title; got != tc.want {
				t.Errorf("title = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestEachShareGetsItsOwnRemoteFile(t *testing.T) {
	// A remote file records the channel, timestamp and author of every share it
	// backs, and files.remote.add upserts, so one file per screenshot would both
	// expose each share to viewers of the others and rewrite already-posted
	// cards. Every share therefore registers its own file.
	srv := pngServer(t, testPNG(t, 800, 600))
	defer srv.Close()

	api := &fakeAPI{}
	b := newTestBridge(t, api, baseConfig(srv.URL))

	first := linkEvent()
	b.HandleLinkShared(context.Background(), "T1", first)

	elsewhere := linkEvent() // the same screenshot, another channel and message
	elsewhere.Channel = "C2"
	elsewhere.MessageTimeStamp = "1700000009.000500"
	b.HandleLinkShared(context.Background(), "T1", elsewhere)

	if len(api.adds) != 2 {
		t.Fatalf("expected 2 files.remote.add, got %d", len(api.adds))
	}
	a, bID := api.adds[0].params.ExternalID, api.adds[1].params.ExternalID
	if a == bID {
		t.Errorf("both shares registered the same external ID %q", a)
	}
	for _, id := range []string{a, bID} {
		if !strings.HasPrefix(id, "abcdef-") {
			t.Errorf("external ID %q should be derived from the image ID", id)
		}
	}
	// Both cards still link to the one screenshot page.
	for i, add := range api.adds {
		if want := srv.URL + "/abcdef"; add.params.ExternalURL != want {
			t.Errorf("add %d external URL = %q, want %q", i, add.params.ExternalURL, want)
		}
	}
}

func TestRedeliveredEventReusesTheSameRemoteFile(t *testing.T) {
	// Slack redelivers events and re-fires on an edited message; the ID is
	// derived from the message so those refresh the card instead of piling up
	// duplicate files.
	srv := pngServer(t, testPNG(t, 800, 600))
	defer srv.Close()

	api := &fakeAPI{}
	b := newTestBridge(t, api, baseConfig(srv.URL))
	b.HandleLinkShared(context.Background(), "T1", linkEvent())
	b.HandleLinkShared(context.Background(), "T1", linkEvent()) // same event again

	if len(api.adds) != 2 {
		t.Fatalf("expected 2 adds, got %d", len(api.adds))
	}
	if api.adds[0].params.ExternalID != api.adds[1].params.ExternalID {
		t.Errorf("the same share used two external IDs: %q and %q",
			api.adds[0].params.ExternalID, api.adds[1].params.ExternalID)
	}
}

func TestPreviewTitleTruncatesLongTitles(t *testing.T) {
	// The server does not cap stored titles, so the bridge must.
	long := strings.Repeat("é", maxTitleLen+50) // multi-byte: truncation is by rune
	got := previewTitle("abcdef", imageMeta{title: long})
	if n := len([]rune(got)); n != maxTitleLen {
		t.Errorf("truncated title has %d runes, want %d", n, maxTitleLen)
	}
	if !strings.HasPrefix(long, got) {
		t.Error("truncated title should be a prefix of the original")
	}
}

func TestPreviewIsEnlargedToSlacksMinimum(t *testing.T) {
	// A screenshot below Slack's 300px floor would be rejected by
	// files.remote.add, so the preview must come out at least that big.
	srv := pngServer(t, testPNG(t, 40, 30))
	defer srv.Close()

	api := &fakeAPI{}
	b := newTestBridge(t, api, baseConfig(srv.URL))
	b.HandleLinkShared(context.Background(), "T1", linkEvent())

	if len(api.adds) != 1 {
		t.Fatalf("expected 1 files.remote.add, got %d", len(api.adds))
	}
	w, h := previewDims(t, api.adds[0].preview)
	if w < imageproc.MinPreviewDim || h < imageproc.MinPreviewDim {
		t.Errorf("preview is %dx%d, want at least %d on each side", w, h, imageproc.MinPreviewDim)
	}
}

func TestPreviewIsCappedAtMaxDimension(t *testing.T) {
	srv := pngServer(t, testPNG(t, 4000, 2000))
	defer srv.Close()

	api := &fakeAPI{}
	cfg := baseConfig(srv.URL)
	cfg.MaxDimension = 1000
	b := newTestBridge(t, api, cfg)
	b.HandleLinkShared(context.Background(), "T1", linkEvent())

	w, h := previewDims(t, api.adds[0].preview)
	if w != 1000 || h != 500 {
		t.Errorf("preview is %dx%d, want 1000x500 (capped, aspect preserved)", w, h)
	}
}

func TestAddFailureLeavesTheLinkUnfurled(t *testing.T) {
	srv := pngServer(t, testPNG(t, 800, 600))
	defer srv.Close()

	// There is no fallback rendering: a Slack error on add means the link stays
	// a plain URL, and nothing else is attempted.
	api := &fakeAPI{addErr: io.ErrUnexpectedEOF}
	b := newTestBridge(t, api, baseConfig(srv.URL))
	b.HandleLinkShared(context.Background(), "T1", linkEvent())

	if api.addCount() != 1 {
		t.Errorf("expected the single failed add, got %d", api.addCount())
	}
	if api.unfurls != 0 {
		t.Errorf("expected no unfurl after a failed add, got %d", api.unfurls)
	}
}

func TestWaitsForTheRichPreviewBeforeUnfurling(t *testing.T) {
	// Slack parses the preview asynchronously; unfurling before it is ready
	// leaves the card blank for far longer, so the bridge polls first.
	srv := pngServer(t, testPNG(t, 800, 600))
	defer srv.Close()

	api := &fakeAPI{richAfter: 2} // ready on the third files.remote.info
	cfg := baseConfig(srv.URL)
	cfg.PreviewWait = config.TOMLDuration{Duration: 5 * time.Second}
	b := newTestBridge(t, api, cfg)
	b.HandleLinkShared(context.Background(), "T1", linkEvent())

	if api.infoCount() != 3 {
		t.Errorf("expected polling to stop at the third info call, got %d", api.infoCount())
	}
	if api.unfurls != 1 {
		t.Errorf("expected the unfurl after the preview became ready, got %d", api.unfurls)
	}
}

func TestPreviewWaitIsSharedAcrossTheMessagesLinks(t *testing.T) {
	// Every screenshot is registered before any waiting starts, so Slack parses
	// the previews concurrently and the message costs one wait, not one per link.
	srv := pngServer(t, testPNG(t, 800, 600))
	defer srv.Close()

	api := &fakeAPI{}
	cfg := baseConfig(srv.URL)
	cfg.PreviewWait = config.TOMLDuration{Duration: 5 * time.Second}
	b := newTestBridge(t, api, cfg)

	ev := linkEvent()
	ev.Links = nil
	for _, id := range []string{"aaaa", "bbbb", "cccc"} {
		ev.Links = append(ev.Links, slackevents.SharedLinks{
			Domain: "screen.corp.example",
			URL:    "https://screen.corp.example/" + id,
		})
	}
	// The previews report ready on the first poll, so a shared wait needs one
	// info call per file; a per-link wait would interleave adds and polls.
	b.HandleLinkShared(context.Background(), "T1", ev)

	if len(api.adds) != 3 || api.unfurls != 3 {
		t.Fatalf("expected 3 adds and 3 unfurls, got %d and %d", len(api.adds), api.unfurls)
	}
	if got := api.firstPollAfter; got != 3 {
		t.Errorf("waiting began after %d adds, want all 3 registered first", got)
	}
}

func TestUnfurlsAnywayWhenThePreviewNeverBecomesReady(t *testing.T) {
	srv := pngServer(t, testPNG(t, 800, 600))
	defer srv.Close()

	api := &fakeAPI{richAfter: 1 << 30} // never ready
	cfg := baseConfig(srv.URL)
	cfg.PreviewWait = config.TOMLDuration{Duration: 300 * time.Millisecond}
	b := newTestBridge(t, api, cfg)
	b.HandleLinkShared(context.Background(), "T1", linkEvent())

	if api.unfurls != 1 {
		t.Errorf("expected the unfurl to proceed after preview_wait elapsed, got %d", api.unfurls)
	}
}

func TestSkipsThePreviewWaitWhenDisabled(t *testing.T) {
	srv := pngServer(t, testPNG(t, 800, 600))
	defer srv.Close()

	api := &fakeAPI{richAfter: 1 << 30}
	b := newTestBridge(t, api, baseConfig(srv.URL)) // preview_wait = 0
	b.HandleLinkShared(context.Background(), "T1", linkEvent())

	if api.infoCount() != 0 {
		t.Errorf("preview_wait = 0 should not poll, got %d info calls", api.infoCount())
	}
	if api.unfurls != 1 {
		t.Errorf("expected 1 unfurl, got %d", api.unfurls)
	}
}

func TestRenderFetchesWithNoRedirect(t *testing.T) {
	var gotURI string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotURI = r.RequestURI
		w.Header().Set("Content-Type", "image/png")
		w.Write(testPNG(t, 800, 600))
	}))
	defer srv.Close()

	b := newTestBridge(t, &fakeAPI{}, baseConfig(srv.URL))
	b.HandleLinkShared(context.Background(), "T1", linkEvent())

	if gotURI != "/abcdef.png?no_redirect=1" {
		t.Errorf("fetched %q, want the PNG path with no_redirect=1 so the server does not 301 to its canonical address", gotURI)
	}
}

func TestRenderSkipsMissingImage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()

	api := &fakeAPI{}
	b := newTestBridge(t, api, baseConfig(srv.URL))
	ev := linkEvent()
	ev.Links[0].URL = "https://screen.corp.example/missing"
	b.HandleLinkShared(context.Background(), "T1", ev)

	if api.addCount() != 0 || api.unfurls != 0 {
		t.Errorf("expected no Slack calls for missing image, got %d uploads %d unfurls", api.addCount(), api.unfurls)
	}
}

func TestRenderRejectsNonImageContentType(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte("<html>not an image</html>"))
	}))
	defer srv.Close()

	api := &fakeAPI{}
	b := newTestBridge(t, api, baseConfig(srv.URL))
	b.HandleLinkShared(context.Background(), "T1", linkEvent())

	if api.addCount() != 0 {
		t.Errorf("expected no upload for non-image content-type, got %d", api.addCount())
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

	api := &fakeAPI{}
	b := newTestBridge(t, api, baseConfig(srv.URL))
	b.HandleLinkShared(context.Background(), "T1", linkEvent())

	if hitTarget {
		t.Error("redirect was followed; the bridge should refuse redirects")
	}
	if api.addCount() != 0 {
		t.Errorf("expected no upload after a refused redirect, got %d", api.addCount())
	}
}

func TestHandleLinkSharedSkipsComposePreview(t *testing.T) {
	api := &fakeAPI{}
	b := newTestBridge(t, api, baseConfig("http://unused"))
	ev := linkEvent()
	ev.MessageTimeStamp = "a1b2c3d4-e5f6-7890-abcd-ef1234567890" // UUID: compose area
	b.HandleLinkShared(context.Background(), "T1", ev)
	if api.addCount() != 0 || api.unfurls != 0 {
		t.Errorf("compose-area preview should be skipped, got %d uploads %d unfurls", api.addCount(), api.unfurls)
	}
}

func TestHandleLinkSharedCapsLinksPerMessage(t *testing.T) {
	srv := pngServer(t, testPNG(t, 10, 10))
	defer srv.Close()

	api := &fakeAPI{}
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
	if api.addCount() != 2 {
		t.Errorf("expected 2 uploads (cap=2), got %d", api.addCount())
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

	api := &fakeAPI{}
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
	api := &fakeAPI{addHook: func() {
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

	if api.addCount() != 1 {
		t.Errorf("expected only the first event to upload (1), got %d; the second should have been dropped", api.addCount())
	}
}

func TestAuthzBlocksExternalUsers(t *testing.T) {
	srv := pngServer(t, testPNG(t, 10, 10))
	defer srv.Close()

	api := &fakeAPI{
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
		user     string
		wantAdds int
	}{
		{"Uguest", 0},
		{"Uextern", 0},
		{"Uok", 1},
	} {
		api.adds = nil
		ev := linkEvent()
		ev.User = tc.user
		b.HandleLinkShared(context.Background(), "T1", ev)
		if api.addCount() != tc.wantAdds {
			t.Errorf("user %q: expected %d uploads, got %d", tc.user, tc.wantAdds, api.addCount())
		}
	}
}

func TestAuthzFailsClosedOnEmptyTeamID(t *testing.T) {
	srv := pngServer(t, testPNG(t, 10, 10))
	defer srv.Close()

	// A user who would be internal in workspace T1, but the event carries no
	// team ID, so the bridge cannot confirm membership and must deny.
	api := &fakeAPI{users: map[string]*slack.User{"Uok": {ID: "Uok", TeamID: "T1"}}}
	cfg := baseConfig(srv.URL)
	cfg.BlockExternalUsers = true
	b := newTestBridge(t, api, cfg)

	ev := linkEvent()
	ev.User = "Uok"
	b.HandleLinkShared(context.Background(), "", ev) // empty teamID
	if api.addCount() != 0 {
		t.Errorf("expected fail-closed (no uploads) when team ID is empty, got %d", api.addCount())
	}
}

func TestAuthzRestrictsToAllowedGroups(t *testing.T) {
	srv := pngServer(t, testPNG(t, 10, 10))
	defer srv.Close()

	api := &fakeAPI{groupMembers: map[string][]string{"S1": {"Umember"}}}
	cfg := baseConfig(srv.URL)
	cfg.AllowedUserGroups = []string{"S1"}
	b := newTestBridge(t, api, cfg)

	for _, tc := range []struct {
		user     string
		wantAdds int
	}{
		{"Umember", 1},
		{"Ustranger", 0},
	} {
		api.adds = nil
		ev := linkEvent()
		ev.User = tc.user
		b.HandleLinkShared(context.Background(), "T1", ev)
		if api.addCount() != tc.wantAdds {
			t.Errorf("user %q: expected %d uploads, got %d", tc.user, tc.wantAdds, api.addCount())
		}
	}
}

func TestAuthzFailsClosedOnAPIError(t *testing.T) {
	srv := pngServer(t, testPNG(t, 10, 10))
	defer srv.Close()

	api := &fakeAPI{userErr: io.ErrUnexpectedEOF}
	cfg := baseConfig(srv.URL)
	cfg.BlockExternalUsers = true
	b := newTestBridge(t, api, cfg)

	b.HandleLinkShared(context.Background(), "T1", linkEvent())
	if api.addCount() != 0 {
		t.Errorf("expected no uploads when user lookup fails (fail closed), got %d", api.addCount())
	}
}
