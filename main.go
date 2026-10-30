package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const (
	defaultMaxBytes = 20 * 1024 * 1024
	userAgent       = "gh-user-attachment/1.0"
)

var assetIDPattern = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

type config struct {
	apiBaseURL string
	host       string
	maxBytes   int64
	outputPath string
	repository string
	timeout    time.Duration
}

type resolver struct {
	apiBaseURL *url.URL
	client     *http.Client
	maxBytes   int64
	token      string
}

type searchPayload struct {
	Items []issueReference `json:"items"`
}

type issueReference struct {
	URL string `json:"url"`
}

type issuePayload struct {
	BodyHTML    string `json:"body_html"`
	Comments    int    `json:"comments"`
	CommentsURL string `json:"comments_url"`
}

type commentPayload struct {
	BodyHTML string `json:"body_html"`
}

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "gh user-attachment: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr io.Writer) error {
	cfg, reference, err := parseFlags(args, stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}

	token := firstNonEmpty(os.Getenv("GH_TOKEN"), os.Getenv("GITHUB_TOKEN"))
	if token == "" {
		return errors.New("GH_TOKEN or GITHUB_TOKEN is required")
	}

	assetID, err := parseAttachmentReference(reference, cfg.host)
	if err != nil {
		return err
	}

	apiBaseURL, err := parseAPIBaseURL(cfg.apiBaseURL)
	if err != nil {
		return err
	}

	client := &http.Client{
		Timeout: cfg.timeout,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	r := resolver{
		apiBaseURL: apiBaseURL,
		client:     client,
		maxBytes:   cfg.maxBytes,
		token:      token,
	}

	signedURL, err := r.resolve(assetID, cfg.repository)
	if err != nil {
		return err
	}

	body, mimeType, err := r.download(signedURL)
	if err != nil {
		return err
	}

	if cfg.outputPath == "" {
		extension, err := attachmentExtension(mimeType)
		if err != nil {
			return err
		}
		cfg.outputPath = assetID + extension
	}
	cfg.outputPath, err = filepath.Abs(cfg.outputPath)
	if err != nil {
		return fmt.Errorf("resolve output path: %w", err)
	}

	parent := filepath.Dir(cfg.outputPath)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return fmt.Errorf("create output directory: %w", err)
	}
	if err := os.WriteFile(cfg.outputPath, body, 0o600); err != nil {
		return fmt.Errorf("write attachment: %w", err)
	}

	fmt.Fprintf(
		stdout,
		"Downloaded GitHub user attachment %s to %s (%s, %d bytes)\n",
		assetID,
		cfg.outputPath,
		mimeType,
		len(body),
	)
	return nil
}

func parseFlags(args []string, stderr io.Writer) (config, string, error) {
	var cfg config
	var maxMiB int
	var timeoutSeconds int

	fs := flag.NewFlagSet("gh user-attachment", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&cfg.repository, "repo", os.Getenv("GITHUB_REPOSITORY"), "repository containing the attachment, in owner/name form")
	fs.StringVar(&cfg.host, "host", firstNonEmpty(os.Getenv("GH_HOST"), "github.com"), "GitHub hostname")
	fs.StringVar(&cfg.apiBaseURL, "api-url", os.Getenv("GITHUB_API_URL"), "GitHub REST API base URL")
	fs.StringVar(&cfg.outputPath, "output", "", "write the attachment to this path (default: <GUID>.<extension> in the current directory)")
	fs.IntVar(&maxMiB, "max-size", 20, "maximum attachment size in MiB")
	fs.IntVar(&timeoutSeconds, "timeout", 30, "request timeout in seconds")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "Download a private GitHub user attachment using repository issue or comment metadata.")
		fmt.Fprintln(stderr)
		fmt.Fprintln(stderr, "Usage:")
		fmt.Fprintln(stderr, "  gh user-attachment [flags] <attachment-url-or-guid>")
		fmt.Fprintln(stderr)
		fmt.Fprintln(stderr, "Flags:")
		fs.PrintDefaults()
	}

	if err := fs.Parse(args); err != nil {
		return config{}, "", err
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return config{}, "", errors.New("exactly one attachment URL or GUID is required")
	}
	if !validRepository(cfg.repository) {
		return config{}, "", errors.New("repository must use owner/name format; pass --repo or set GITHUB_REPOSITORY")
	}

	host, err := normaliseHost(cfg.host)
	if err != nil {
		return config{}, "", err
	}
	cfg.host = host

	if cfg.apiBaseURL == "" {
		if cfg.host == "github.com" {
			cfg.apiBaseURL = "https://api.github.com"
		} else {
			cfg.apiBaseURL = "https://api." + cfg.host
		}
	}
	if timeoutSeconds <= 0 {
		return config{}, "", errors.New("timeout must be greater than zero")
	}
	if maxMiB <= 0 {
		return config{}, "", errors.New("max-size must be greater than zero")
	}

	cfg.timeout = time.Duration(timeoutSeconds) * time.Second
	cfg.maxBytes = int64(maxMiB) * 1024 * 1024
	return cfg, fs.Arg(0), nil
}

func (r resolver) resolve(assetID, repository string) (string, error) {
	query := url.Values{
		"q":        {assetID + " repo:" + repository},
		"per_page": {"10"},
	}
	searchURL := r.apiURL("/search/issues")
	searchURL.RawQuery = query.Encode()

	var search searchPayload
	if _, err := r.getJSON(searchURL.String(), "application/vnd.github+json", &search); err != nil {
		return "", err
	}

	for _, item := range search.Items {
		var issue issuePayload
		if _, err := r.getJSON(item.URL, "application/vnd.github.html+json", &issue); err != nil {
			return "", err
		}
		if signedURL := findSignedAttachmentURL(issue.BodyHTML, assetID); signedURL != "" {
			return signedURL, nil
		}

		if issue.Comments == 0 || issue.CommentsURL == "" {
			continue
		}

		commentsURL := issue.CommentsURL
		for page := 0; commentsURL != "" && page < 20; page++ {
			parsedCommentsURL, err := url.Parse(commentsURL)
			if err != nil {
				return "", fmt.Errorf("parse comments URL: %w", err)
			}
			values := parsedCommentsURL.Query()
			values.Set("per_page", "100")
			parsedCommentsURL.RawQuery = values.Encode()

			var comments []commentPayload
			response, err := r.getJSON(parsedCommentsURL.String(), "application/vnd.github.html+json", &comments)
			if err != nil {
				return "", err
			}
			for _, comment := range comments {
				if signedURL := findSignedAttachmentURL(comment.BodyHTML, assetID); signedURL != "" {
					return signedURL, nil
				}
			}
			commentsURL = nextPageURL(response.Header.Get("Link"))
		}
	}

	return "", fmt.Errorf("attachment %s was not found in repository %s", assetID, repository)
}

func (r resolver) getJSON(rawURL, accept string, destination any) (*http.Response, error) {
	if !r.isAPIURL(rawURL) {
		return nil, errors.New("refusing to send GitHub credentials outside the configured API")
	}

	request, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("create GitHub API request: %w", err)
	}
	request.Header.Set("Accept", accept)
	request.Header.Set("Authorization", "Bearer "+r.token)
	request.Header.Set("User-Agent", userAgent)

	response, err := r.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("GitHub API request: %w", err)
	}
	defer response.Body.Close()

	body, err := readLimited(response.Body, 2*1024*1024)
	if err != nil {
		return nil, fmt.Errorf("read GitHub API response: %w", err)
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf(
			"GitHub API request failed with HTTP %d: %s",
			response.StatusCode,
			strings.TrimSpace(string(body)),
		)
	}
	if err := json.Unmarshal(body, destination); err != nil {
		return nil, fmt.Errorf("decode GitHub API response: %w", err)
	}
	return response, nil
}

func (r resolver) download(rawURL string) ([]byte, string, error) {
	parsedURL, err := url.Parse(rawURL)
	if err != nil || parsedURL.Scheme != "https" || parsedURL.Host == "" {
		return nil, "", errors.New("resolved attachment URL must use HTTPS")
	}

	client := *r.client
	client.CheckRedirect = nil
	request, err := http.NewRequest(http.MethodGet, parsedURL.String(), nil)
	if err != nil {
		return nil, "", fmt.Errorf("create attachment request: %w", err)
	}
	request.Header.Set("Accept", "application/octet-stream")
	request.Header.Set("User-Agent", userAgent)

	response, err := client.Do(request)
	if err != nil {
		return nil, "", fmt.Errorf("download attachment: %w", err)
	}
	defer response.Body.Close()

	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		body, _ := readLimited(response.Body, 2048)
		return nil, "", fmt.Errorf(
			"attachment download failed with HTTP %d: %s",
			response.StatusCode,
			strings.TrimSpace(string(body)),
		)
	}

	mimeType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil {
		return nil, "", fmt.Errorf("parse attachment Content-Type: %w", err)
	}
	mimeType = normaliseContentType(mimeType)
	if mimeType == "text/html" || mimeType == "application/xhtml+xml" {
		return nil, "", fmt.Errorf("attachment response was %s, not attachment content", mimeType)
	}
	if response.ContentLength > r.maxBytes {
		return nil, "", fmt.Errorf("attachment exceeds the %d-byte size limit", r.maxBytes)
	}

	body, err := readLimited(response.Body, r.maxBytes)
	if err != nil {
		return nil, "", fmt.Errorf("read attachment: %w", err)
	}
	return body, mimeType, nil
}

func normaliseContentType(mimeType string) string {
	switch mimeType {
	case "image/jpg", "image/pjpeg":
		return "image/jpeg"
	case "image/x-png":
		return "image/png"
	case "document/pdf":
		return "application/pdf"
	default:
		return mimeType
	}
}

func attachmentExtension(mimeType string) (string, error) {
	switch mimeType {
	case "image/jpeg":
		return ".jpg", nil
	case "image/tiff":
		return ".tiff", nil
	case "image/x-icon", "image/vnd.microsoft.icon":
		return ".ico", nil
	case "image/avif":
		return ".avif", nil
	case "application/pdf":
		return ".pdf", nil
	case "text/plain":
		return ".txt", nil
	case "text/csv", "application/csv":
		return ".csv", nil
	case "application/json":
		return ".json", nil
	case "application/xml", "text/xml":
		return ".xml", nil
	case "application/zip", "application/x-zip-compressed":
		return ".zip", nil
	case "application/gzip", "application/x-gzip":
		return ".gz", nil
	case "application/octet-stream":
		return ".bin", nil
	case "audio/mpeg":
		return ".mp3", nil
	case "video/mp4":
		return ".mp4", nil
	}
	extensions, err := mime.ExtensionsByType(mimeType)
	if err != nil {
		return "", fmt.Errorf("derive attachment extension: %w", err)
	}
	if len(extensions) == 0 {
		return "", fmt.Errorf("cannot derive a file extension for %s; pass --output to specify a filename", mimeType)
	}
	return extensions[0], nil
}

func parseAttachmentReference(reference, host string) (string, error) {
	value := strings.TrimSpace(reference)
	if assetIDPattern.MatchString(value) {
		return value, nil
	}

	parsedURL, err := url.Parse(value)
	if err != nil {
		return "", errors.New("attachment reference must be a GUID or complete HTTPS URL")
	}
	if parsedURL.Scheme != "https" {
		return "", errors.New("attachment URL must use HTTPS")
	}
	if parsedURL.User != nil || parsedURL.Port() != "" {
		return "", errors.New("attachment URL must not contain credentials or a custom port")
	}
	if !strings.EqualFold(parsedURL.Hostname(), host) {
		return "", fmt.Errorf("attachment URL host must be %s", host)
	}

	const prefix = "/user-attachments/assets/"
	if !strings.HasPrefix(parsedURL.EscapedPath(), prefix) {
		return "", errors.New("attachment URL must match /user-attachments/assets/<GUID>")
	}
	assetID, err := url.PathUnescape(strings.TrimSuffix(strings.TrimPrefix(parsedURL.EscapedPath(), prefix), "/"))
	if err != nil || !assetIDPattern.MatchString(assetID) {
		return "", errors.New("attachment URL must end with a valid asset GUID")
	}
	if parsedURL.RawQuery != "" || parsedURL.Fragment != "" {
		return "", errors.New("attachment URL must not contain a query string or fragment")
	}
	return assetID, nil
}

func parseAPIBaseURL(value string) (*url.URL, error) {
	parsedURL, err := url.Parse(strings.TrimRight(strings.TrimSpace(value), "/"))
	if err != nil || parsedURL.Scheme != "https" || parsedURL.Host == "" {
		return nil, errors.New("API URL must be a complete HTTPS URL")
	}
	if parsedURL.User != nil || parsedURL.RawQuery != "" || parsedURL.Fragment != "" {
		return nil, errors.New("API URL must not contain credentials, a query string, or a fragment")
	}
	return parsedURL, nil
}

func (r resolver) apiURL(path string) *url.URL {
	result := *r.apiBaseURL
	result.Path = strings.TrimRight(r.apiBaseURL.Path, "/") + path
	return &result
}

func (r resolver) isAPIURL(rawURL string) bool {
	candidate, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	basePath := strings.TrimRight(r.apiBaseURL.Path, "/")
	return candidate.Scheme == r.apiBaseURL.Scheme &&
		candidate.Host == r.apiBaseURL.Host &&
		(candidate.Path == basePath || strings.HasPrefix(candidate.Path, basePath+"/"))
}

func findSignedAttachmentURL(bodyHTML, assetID string) string {
	decoded := html.UnescapeString(bodyHTML)
	matches := regexp.MustCompile(`https://[^"'\s<>]+`).FindAllString(decoded, -1)
	for _, match := range matches {
		if strings.Contains(match, assetID) {
			return match
		}
	}
	return ""
}

func nextPageURL(linkHeader string) string {
	for _, entry := range strings.Split(linkHeader, ",") {
		parts := strings.Split(strings.TrimSpace(entry), ";")
		if len(parts) < 2 || !strings.Contains(parts[1], `rel="next"`) {
			continue
		}
		return strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(parts[0]), "<"), ">")
	}
	return ""
}

func readLimited(reader io.Reader, limit int64) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("response exceeds the %d-byte size limit", limit)
	}
	return body, nil
}

func normaliseHost(value string) (string, error) {
	host := strings.ToLower(strings.TrimSpace(value))
	if host == "" || strings.ContainsAny(host, "/@:") {
		return "", fmt.Errorf("invalid GitHub host: %s", value)
	}
	return host, nil
}

func validRepository(value string) bool {
	return regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`).MatchString(value)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
