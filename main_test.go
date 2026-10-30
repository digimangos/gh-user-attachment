package main

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testAssetID = "78e4a4a7-399a-4faf-b20f-eff57aee7202"

func TestParseAttachmentReference(t *testing.T) {
	t.Parallel()

	assetID, err := parseAttachmentReference(
		"https://github.example.com/user-attachments/assets/"+testAssetID,
		"github.example.com",
	)
	if err != nil {
		t.Fatal(err)
	}
	if assetID != testAssetID {
		t.Fatalf("got %q, want %q", assetID, testAssetID)
	}

	_, err = parseAttachmentReference(
		"https://example.com/user-attachments/assets/"+testAssetID,
		"github.example.com",
	)
	if err == nil {
		t.Fatal("expected a different host to be rejected")
	}
}

func TestFindSignedAttachmentURL(t *testing.T) {
	t.Parallel()

	body := `<img src="https://objects.example.test/` + testAssetID + `.png?signature=test&amp;expires=300">`
	got := findSignedAttachmentURL(body, testAssetID)
	want := "https://objects.example.test/" + testAssetID + ".png?signature=test&expires=300"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestResolverDownloadsImageWithoutForwardingToken(t *testing.T) {
	t.Parallel()

	var server *httptest.Server
	server = httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/search/issues":
			assertBearerToken(t, request)
			fmt.Fprintf(response, `{"items":[{"url":%q}]}`, server.URL+"/repos/owner/repository/issues/3")
		case "/repos/owner/repository/issues/3":
			assertBearerToken(t, request)
			response.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(
				response,
				`{"body_html":%q,"comments":0}`,
				`<img src="`+server.URL+`/objects/`+testAssetID+`.png?signature=test&amp;expires=300">`,
			)
		case "/objects/" + testAssetID + ".png":
			if request.Header.Get("Authorization") != "" {
				t.Fatal("token was forwarded to object storage")
			}
			response.Header().Set("Content-Type", "image/png")
			_, _ = response.Write([]byte("image bytes"))
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()

	apiBaseURL, err := parseAPIBaseURL(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	r := resolver{
		apiBaseURL: apiBaseURL,
		client:     server.Client(),
		maxBytes:   defaultMaxBytes,
		token:      "test-token",
	}

	signedURL, err := r.resolve(testAssetID, "owner/repository")
	if err != nil {
		t.Fatal(err)
	}

	r.client.Timeout = time.Second
	body, mimeType, err := r.download(signedURL)
	if err != nil {
		t.Fatal(err)
	}
	if mimeType != "image/png" {
		t.Fatalf("got MIME type %q", mimeType)
	}
	if string(body) != "image bytes" {
		t.Fatalf("got body %q", body)
	}
}

func TestRunWritesAttachment(t *testing.T) {
	server := newAttachmentServer(t)
	defer server.Close()

	t.Setenv("GH_TOKEN", "test-token")
	t.Setenv("GITHUB_TOKEN", "")

	for _, filename := range []string{"attachment.png", "attachment", "nested/attachment.custom"} {
		t.Run(filename, func(t *testing.T) {
			outputPath := filepath.Join(t.TempDir(), filename)
			var stdout bytes.Buffer
			var stderr bytes.Buffer

			err := run([]string{
				"--repo", "owner/repository",
				"--host", "github.example.com",
				"--api-url", server.URL,
				"--output", outputPath,
				testAssetID,
			}, &stdout, &stderr)
			if err != nil {
				t.Fatalf("run failed: %v\nstderr: %s", err, stderr.String())
			}

			body, err := os.ReadFile(outputPath)
			if err != nil {
				t.Fatal(err)
			}
			if string(body) != "image bytes" {
				t.Fatalf("got body %q", body)
			}
			if !strings.Contains(stdout.String(), "to "+outputPath+" (image/png,") {
				t.Fatalf("unexpected output: %s", stdout.String())
			}
		})
	}
}

func TestRunHelpSucceeds(t *testing.T) {
	t.Parallel()

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if err := run([]string{"--help"}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr.String(), "Usage:") {
		t.Fatalf("unexpected help output: %s", stderr.String())
	}
}

func TestAttachmentExtension(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		mimeType  string
		extension string
	}{
		{"image/png", ".png"},
		{"image/jpeg", ".jpg"},
		{"image/gif", ".gif"},
		{"image/webp", ".webp"},
		{"image/svg+xml", ".svg"},
		{"image/tiff", ".tiff"},
		{"image/avif", ".avif"},
		{"image/vnd.microsoft.icon", ".ico"},
		{"application/pdf", ".pdf"},
		{"text/plain", ".txt"},
		{"text/csv", ".csv"},
		{"application/json", ".json"},
		{"application/xml", ".xml"},
		{"application/zip", ".zip"},
		{"application/gzip", ".gz"},
		{"application/octet-stream", ".bin"},
		{"application/msword", ".doc"},
		{"application/vnd.openxmlformats-officedocument.wordprocessingml.document", ".docx"},
		{"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", ".xlsx"},
		{"application/vnd.openxmlformats-officedocument.presentationml.presentation", ".pptx"},
		{"audio/mpeg", ".mp3"},
		{"video/mp4", ".mp4"},
	} {
		t.Run(tc.mimeType, func(t *testing.T) {
			got, err := attachmentExtension(tc.mimeType)
			if err != nil || got != tc.extension {
				t.Fatalf("got %q, %v; want %q", got, err, tc.extension)
			}
		})
	}
	if _, err := attachmentExtension("image/x-unknown-attachment"); err == nil || !strings.Contains(err.Error(), "--output") {
		t.Fatalf("expected unknown type to require --output, got %v", err)
	}
}

func TestRunDefaultFilename(t *testing.T) {
	server := newAttachmentServer(t)
	defer server.Close()
	t.Setenv("GH_TOKEN", "test-token")
	t.Setenv("GITHUB_TOKEN", "")
	directory := t.TempDir()
	originalDirectory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(directory); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(originalDirectory); err != nil {
			t.Fatal(err)
		}
	})
	var stdout, stderr bytes.Buffer
	if err := run([]string{
		"--repo", "owner/repository", "--api-url", server.URL, testAssetID,
	}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	path, err := filepath.Abs(testAssetID + ".png")
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil || string(body) != "image bytes" {
		t.Fatalf("got body %q, error %v", body, err)
	}
	if !strings.Contains(stdout.String(), "to "+path+" (image/png,") {
		t.Fatalf("output does not report saved path: %s", stdout.String())
	}
}

func TestDownloadUsesFinalContentType(t *testing.T) {
	t.Parallel()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Header.Get("Authorization") != "" {
			t.Error("token was forwarded to object storage")
		}
		if req.URL.Path == "/redirect" {
			w.Header().Set("Content-Type", "text/html")
			http.Redirect(w, req, "/"+testAssetID, http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", "IMAGE/JPEG; charset=binary")
		_, _ = io.WriteString(w, "image bytes")
	}))
	defer server.Close()
	r := resolver{client: server.Client(), maxBytes: defaultMaxBytes, token: "test-token"}
	body, mimeType, err := r.download(server.URL + "/redirect")
	if err != nil || string(body) != "image bytes" || mimeType != "image/jpeg" {
		t.Fatalf("got %q, %q, %v", body, mimeType, err)
	}
	extension, err := attachmentExtension(mimeType)
	if err != nil || extension != ".jpg" {
		t.Fatalf("got extension %q, %v", extension, err)
	}
}

func TestDownloadContentTypes(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		header    string
		mimeType  string
		extension string
	}{
		{"image/jpg", "image/jpeg", ".jpg"},
		{" IMAGE/JPG; charset=binary ", "image/jpeg", ".jpg"},
		{"image/pjpeg", "image/jpeg", ".jpg"},
		{"image/x-png", "image/png", ".png"},
		{"document/pdf", "application/pdf", ".pdf"},
		{"Document/PDF; charset=binary", "application/pdf", ".pdf"},
		{"application/pdf", "application/pdf", ".pdf"},
		{"text/plain; charset=utf-8", "text/plain", ".txt"},
		{"application/zip", "application/zip", ".zip"},
		{"application/octet-stream", "application/octet-stream", ".bin"},
		{"text/html", "", ""},
		{"application/xhtml+xml", "", ""},
		{"image/png; broken", "", ""},
	} {
		t.Run(tc.header, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				w.Header().Set("Content-Type", tc.header)
				_, _ = io.WriteString(w, "attachment bytes")
			}))
			defer server.Close()
			r := resolver{client: server.Client(), maxBytes: defaultMaxBytes}
			body, mimeType, err := r.download(server.URL)
			if tc.mimeType == "" {
				if err == nil {
					t.Fatal("expected unsupported or malformed content type to be rejected")
				}
				return
			}
			if err != nil || mimeType != tc.mimeType || string(body) != "attachment bytes" {
				t.Fatalf("got %q, %q, %v", body, mimeType, err)
			}
			extension, err := attachmentExtension(mimeType)
			if err != nil || extension != tc.extension {
				t.Fatalf("got extension %q, %v; want %q", extension, err, tc.extension)
			}
		})
	}
}

func TestResolverRejectsCredentialExfiltration(t *testing.T) {
	t.Parallel()

	apiBaseURL, err := parseAPIBaseURL("https://api.github.example.com")
	if err != nil {
		t.Fatal(err)
	}
	r := resolver{apiBaseURL: apiBaseURL}
	if r.isAPIURL("https://example.com/repos/owner/repository/issues/3") {
		t.Fatal("expected external API URL to be rejected")
	}
}

func newAttachmentServer(t *testing.T) *httptest.Server {
	t.Helper()

	var server *httptest.Server
	server = httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/search/issues":
			assertBearerToken(t, request)
			fmt.Fprintf(response, `{"items":[{"url":%q}]}`, server.URL+"/repos/owner/repository/issues/3")
		case "/repos/owner/repository/issues/3":
			assertBearerToken(t, request)
			fmt.Fprintf(
				response,
				`{"body_html":%q,"comments":0}`,
				`<img src="`+server.URL+`/objects/`+testAssetID+`.png">`,
			)
		case "/objects/" + testAssetID + ".png":
			response.Header().Set("Content-Type", "image/png")
			_, _ = io.WriteString(response, "image bytes")
		default:
			http.NotFound(response, request)
		}
	}))

	originalTransport := http.DefaultTransport
	http.DefaultTransport = server.Client().Transport
	t.Cleanup(func() {
		http.DefaultTransport = originalTransport
	})
	return server
}

func assertBearerToken(t *testing.T, request *http.Request) {
	t.Helper()
	if got := request.Header.Get("Authorization"); got != "Bearer test-token" {
		t.Fatalf("got authorization %q", got)
	}
}
