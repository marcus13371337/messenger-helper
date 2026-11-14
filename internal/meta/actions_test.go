package meta

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"go.mau.fi/mautrix-meta/pkg/messagix/types"

	"github.com/marcus13371337/messenger-helper/internal/helper"
)

func TestAttachmentsAreOnlyFetchedFromMetasOwnServers(t *testing.T) {
	allowed := []string{"https://scontent.xx.fbcdn.net/v/cat.jpg?oh=1", "https://cdn.fbsbx.com/file.pdf"}
	refused := []string{"http://scontent.xx.fbcdn.net/cat.jpg", "https://evilfbcdn.net/cat.jpg", "https://www.messenger.com/cat.jpg", "https://example.test/fbcdn.net", "file:///etc/passwd", ""}

	for _, url := range allowed {
		if err := checkDownloadURL(url); err != nil {
			t.Fatalf("%q refused: %v", url, err)
		}
	}
	for _, url := range refused {
		if err := checkDownloadURL(url); err == nil {
			t.Fatalf("%q allowed", url)
		}
	}
}

func TestFileTypeComesFromContentThenName(t *testing.T) {
	cases := []struct {
		name string
		data string
		want string
	}{
		{"photo.bin", "\x89PNG\r\n\x1a\n", "image/png"},
		{"notes.pdf", "%PDF-1.7", "application/pdf"},
		{"data.json", `{"a":1}`, "application/json"},
		{"mystery", "\x00\x01\x02", "application/octet-stream"},
	}
	for _, c := range cases {
		if got := contentTypeOf(c.name, []byte(c.data)); got != c.want {
			t.Fatalf("contentTypeOf(%q) = %q, want %q", c.name, got, c.want)
		}
	}
}

func loginCode(t *testing.T, session helper.Session) string {
	t.Helper()
	_, err := Connector(io.Discard)(context.Background(), session, func(helper.Event) {})
	var known *helper.Error
	if !errors.As(err, &known) {
		t.Fatalf("want a plain error, got %v", err)
	}
	return known.Code
}

func TestLoginIsRefusedBeforeTouchingTheNetworkWhenCookiesAreMissing(t *testing.T) {
	if code := loginCode(t, helper.Session{Cookies: map[string]string{"c_user": "100001", "xs": "x"}}); code != helper.CodeBadSession {
		t.Fatalf("code %q", code)
	}
}

func TestLoginSignsInThroughFacebook(t *testing.T) {
	jar, err := cookieJar(helper.Session{Cookies: map[string]string{"c_user": "100001", "xs": "x", "datr": "d"}})
	if err != nil {
		t.Fatal(err)
	}

	if jar.Platform != types.Facebook {
		t.Fatalf("platform %v", jar.Platform)
	}
}

func TestAttachmentRefThatIsNotOursIsRefused(t *testing.T) {
	for _, ref := range []string{"not-base64!", "e30", ""} {
		if _, err := decodeRef(ref); err == nil {
			t.Fatalf("ref %q accepted", ref)
		}
	}
}

type recordingTransport struct{ referer string }

func (r *recordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r.referer = req.Header.Get("Referer")
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("cat")), Header: http.Header{}, Request: req}, nil
}

func TestAttachmentsAreFetchedAsFacebook(t *testing.T) {
	transport := &recordingTransport{}
	original := http.DefaultClient
	http.DefaultClient = &http.Client{Transport: transport}
	t.Cleanup(func() { http.DefaultClient = original })
	ref := attachmentRef{URL: "https://scontent.xx.fbcdn.net/v/cat.jpg", Type: "image/jpeg"}.encode()

	if _, err := (&Client{}).Download(context.Background(), ref, io.Discard); err != nil {
		t.Fatal(err)
	}

	if transport.referer != "https://www.facebook.com/" {
		t.Fatalf("referer %q", transport.referer)
	}
}
