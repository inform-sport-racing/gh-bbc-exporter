package utils

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"
)

const ghUserAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36"

var (
	// bitbucketInlineImagePattern matches Bitbucket's inline image URL format used in PR descriptions.
	bitbucketInlineImagePattern = regexp.MustCompile(
		`https://bitbucket\.org/repo/[^/]+/images/([^)\s"]+)`)

	// uploadTokenPattern extracts GitHub's upload token from the repo page JS payload.
	uploadTokenPattern = regexp.MustCompile(`"uploadToken":"([^"]+)"`)
)

// ImageFixer downloads Bitbucket-hosted inline images from migrated GitHub PR descriptions
// and re-uploads them to GitHub's CDN, then patches the PR bodies in place.
type ImageFixer struct {
	ghToken       string
	ghAPIBase     string // e.g. "https://api.github.com"
	targetOrg     string
	targetRepo    string
	sessionToken  string // Atlassian cloud.session.token cookie
	ghUserSession string // GitHub user_session cookie for user-attachment uploads
	httpClient    *http.Client
	logger        *zap.Logger
	cachedRepoID      int
	cachedToken       string
	cachedSessionClient *http.Client
}

func NewImageFixer(ghToken, ghAPIBase, targetOrg, targetRepo, sessionToken, ghUserSession string, logger *zap.Logger) *ImageFixer {
	return &ImageFixer{
		ghToken:       ghToken,
		ghAPIBase:     ghAPIBase,
		targetOrg:     targetOrg,
		targetRepo:    targetRepo,
		sessionToken:  sessionToken,
		ghUserSession: ghUserSession,
		httpClient:    &http.Client{},
		logger:        logger,
	}
}

// FixImages scans every open and closed PR in the target GitHub repo,
// downloads any Bitbucket-hosted inline images, uploads them to GitHub's
// user-attachments CDN, and patches the PR body with the new URLs.
func (f *ImageFixer) FixImages() error {
	prs, err := f.listAllPRs()
	if err != nil {
		return fmt.Errorf("listing GitHub PRs: %w", err)
	}
	// Count unique images across all PRs upfront so the user knows the scope.
	allImageURLs := make(map[string]bool)
	for _, pr := range prs {
		if pr.Body == "" {
			continue
		}
		for _, u := range bitbucketInlineImagePattern.FindAllString(pr.Body, -1) {
			allImageURLs[u] = true
		}
	}
	f.logger.Info("Found PRs to scan",
		zap.Int("prs", len(prs)),
		zap.Int("unique_images", len(allImageURLs)))

	// Build a cache of Bitbucket URL → GitHub asset URL so we only upload each image once.
	urlCache := make(map[string]string)

	fixed := 0
	uploadCount := 0
	for _, pr := range prs {
		if pr.Body == "" {
			continue
		}

		matches := bitbucketInlineImagePattern.FindAllString(pr.Body, -1)
		if len(matches) == 0 {
			continue
		}

		f.logger.Info("Processing PR", zap.Int("pr", pr.Number))

		newBody := pr.Body
		changed := false

		for _, bbURL := range matches {
			if ghURL, ok := urlCache[bbURL]; ok {
				newBody = strings.ReplaceAll(newBody, bbURL, ghURL)
				changed = true
				continue
			}

			imgData, contentType, err := f.downloadFromBitbucket(bbURL)
			if err != nil {
				f.logger.Warn("Failed to download Bitbucket image",
					zap.String("url", bbURL), zap.Error(err))
				continue
			}

			filename := extractFilename(bbURL)
			f.logger.Info("Uploading image",
				zap.Int("pr", pr.Number),
				zap.Int("upload_number", uploadCount+1),
				zap.String("file", filename))
			ghURL, err := f.uploadToGitHub(filename, contentType, imgData)
			if err != nil {
				f.logger.Warn("Failed to upload image to GitHub",
					zap.String("url", bbURL), zap.Error(err))
				continue
			}

			uploadCount++
			if uploadCount%10 == 0 {
				pauseSecs := 60 + rand.Intn(61) // random between 60 and 120 seconds
				f.logger.Info("Pausing to avoid GitHub rate limits",
					zap.Int("uploads_done", uploadCount),
					zap.Int("pause_seconds", pauseSecs))
				time.Sleep(time.Duration(pauseSecs) * time.Second)
			}

			urlCache[bbURL] = ghURL
			newBody = strings.ReplaceAll(newBody, bbURL, ghURL)
			changed = true
			f.logger.Debug("Migrated inline image",
				zap.Int("pr", pr.Number),
				zap.String("from", bbURL),
				zap.String("to", ghURL))
		}

		if !changed {
			continue
		}

		if err := f.updatePRBody(pr.Number, newBody); err != nil {
			f.logger.Warn("Failed to update PR body",
				zap.Int("pr", pr.Number), zap.Error(err))
			continue
		}
		fixed++
		f.logger.Info("Updated PR with migrated images", zap.Int("pr", pr.Number))
	}

	f.logger.Info("Image migration complete",
		zap.Int("prs_updated", fixed),
		zap.Int("images_uploaded", len(urlCache)))
	return nil
}

// githubPR is the subset of fields we need from the GitHub REST PR response.
type githubPR struct {
	Number int    `json:"number"`
	Body   string `json:"body"`
}

func (f *ImageFixer) listAllPRs() ([]githubPR, error) {
	var all []githubPR
	pageNum := 1
	for {
		u := fmt.Sprintf("%s/repos/%s/%s/pulls?state=all&per_page=100&page=%d",
			f.ghAPIBase, f.targetOrg, f.targetRepo, pageNum)
		req, err := http.NewRequest("GET", u, nil)
		if err != nil {
			return nil, err
		}
		f.setGitHubAuth(req)

		resp, err := f.httpClient.Do(req)
		if err != nil {
			return nil, err
		}
		respBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("GET %s: %s", u, resp.Status)
		}

		var batch []githubPR
		if err := json.Unmarshal(respBody, &batch); err != nil {
			return nil, err
		}
		all = append(all, batch...)
		if len(batch) < 100 {
			break
		}
		pageNum++
	}
	return all, nil
}

func (f *ImageFixer) downloadFromBitbucket(imgURL string) ([]byte, string, error) {
	// Bitbucket image URLs (bitbucket.org/repo/{uuid}/images/...) redirect to a
	// short-lived pre-signed CDN URL (bytebucket.org or S3).  We must NOT let
	// http.Client follow the redirect automatically because:
	//   1. The redirect target is the pre-signed URL — no auth header needed.
	//   2. Forwarding an Authorization header to S3 causes "conflicting auth" errors.
	//
	// Strategy:
	//   a. Try without auth first (public repos, or if the CDN URL is embargoed
	//      without a prior auth redirect — get the Location directly).
	//   b. If the server requires auth to issue the redirect (401/403/404), retry
	//      with Bitbucket credentials to obtain the Location header.
	//   c. Then GET the pre-signed CDN URL with no auth headers.

	noFollowClient := &http.Client{
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse // stop at first redirect
		},
	}

	// The bitbucket.org image endpoint requires a browser session cookie —
	// workspace/API tokens don't work here. Provide --bitbucket-session-token for reliable
	// image downloads (copy cloud.session.token from browser dev tools).
	type authFn func(*http.Request)
	authAttempts := []authFn{
		func(r *http.Request) {}, // unauthenticated (public repos)
	}
	if f.sessionToken != "" {
		authAttempts = append([]authFn{func(r *http.Request) {
			r.AddCookie(&http.Cookie{Name: "cloud.session.token", Value: f.sessionToken})
		}}, authAttempts...)
	}

	var downloadURL string
	var lastErr error
	for _, applyAuth := range authAttempts {
		downloadURL, lastErr = f.resolveBitbucketRedirect(noFollowClient, imgURL, applyAuth)
		if lastErr == nil {
			break
		}
	}
	if lastErr != nil {
		return nil, "", fmt.Errorf(
			"%w — bitbucket.org image URLs require a browser session cookie; "+
				"retry with --bitbucket-session-token (copy cloud.session.token from browser dev tools)", lastErr)
	}

	// Download the (possibly pre-signed CDN) URL without extra auth headers.
	cdnReq, err := http.NewRequest("GET", downloadURL, nil)
	if err != nil {
		return nil, "", err
	}
	resp, err := f.httpClient.Do(cdnReq)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("HTTP %d downloading %s", resp.StatusCode, downloadURL)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", err
	}

	ct := resp.Header.Get("Content-Type")
	if ct == "" || !strings.HasPrefix(ct, "image/") {
		ct = contentTypeFromFilename(imgURL)
	}
	if idx := strings.Index(ct, ";"); idx != -1 {
		ct = strings.TrimSpace(ct[:idx])
	}
	return data, ct, nil
}

// resolveBitbucketRedirect GETs imgURL and returns either the redirect Location
// or the original URL if the server responds 200 directly.
// applyAuth sets any credentials on the request before it is sent.
func (f *ImageFixer) resolveBitbucketRedirect(client *http.Client, imgURL string, applyAuth func(*http.Request)) (string, error) {
	req, err := http.NewRequest("GET", imgURL, nil)
	if err != nil {
		return "", err
	}
	applyAuth(req)

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		// Server sent the image directly (no redirect needed).
		return imgURL, nil
	case http.StatusMovedPermanently, http.StatusFound,
		http.StatusSeeOther, http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
		loc := resp.Header.Get("Location")
		if loc == "" {
			return "", fmt.Errorf("redirect with no Location header")
		}
		return loc, nil
	default:
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
}

// uploadPolicyResponse is the JSON response from GitHub's /upload/policies/assets endpoint.
type uploadPolicyResponse struct {
	UploadURL string `json:"upload_url"`
	Asset     struct {
		ID          int    `json:"id"`
		Name        string `json:"name"`
		Size        int64  `json:"size"`
		ContentType string `json:"content_type"`
		Href        string `json:"href"`
	} `json:"asset"`
	Form                         map[string]string `json:"form"`
	AssetUploadAuthenticityToken string            `json:"asset_upload_authenticity_token"`
}

func (f *ImageFixer) uploadToGitHub(filename, contentType string, imgData []byte) (string, error) {
	return f.uploadAsUserAttachment(filename, contentType, imgData)
}

// uploadAsUserAttachment uploads via GitHub's internal upload API, producing
// stable github.com/user-attachments/assets/... URLs.
//
// Flow (reverse-engineered from GitHub's web UI):
//  1. GET repo page → extract uploadToken from JS payload
//  2. POST /upload/policies/assets (multipart) → get S3 presigned form
//  3. POST to S3 presigned URL with image data
//  4. PUT /upload/assets/{id} to finalise — mandatory, or the URL returns 404
func (f *ImageFixer) uploadAsUserAttachment(filename, contentType string, imgData []byte) (string, error) {
	if f.ghUserSession == "" {
		return "", fmt.Errorf("--github-user-session is required for user-attachment uploads " +
			"(copy user_session cookie from browser dev tools → Application → Cookies → github.com)")
	}

	if f.cachedSessionClient == nil {
		f.cachedSessionClient = f.newSessionClient()
	}
	sessionClient := f.cachedSessionClient

	if f.cachedRepoID == 0 {
		repoID, err := f.getRepoID()
		if err != nil {
			return "", fmt.Errorf("getting repo ID: %w", err)
		}
		f.cachedRepoID = repoID
	}

	if f.cachedToken == "" {
		uploadToken, err := f.getUploadToken(sessionClient)
		if err != nil {
			return "", fmt.Errorf("getting upload token: %w", err)
		}
		f.cachedToken = uploadToken
	}

	policy, err := f.requestUploadPolicy(sessionClient, f.cachedToken, f.cachedRepoID, filename, contentType, len(imgData))
	if err != nil {
		return "", fmt.Errorf("requesting upload policy: %w", err)
	}

	if err := f.uploadToS3(policy, filename, contentType, imgData); err != nil {
		return "", fmt.Errorf("uploading to S3: %w", err)
	}

	assetURL, err := f.finalizeUpload(sessionClient, policy)
	if err != nil {
		return "", fmt.Errorf("finalizing upload: %w", err)
	}

	return assetURL, nil
}

// newSessionClient returns an http.Client with the GitHub user_session cookie
// set in a cookie jar. GitHub also requires __Host-user_session_same_site
// (same value) for CSRF validation on the upload endpoints.
func (f *ImageFixer) newSessionClient() *http.Client {
	jar, _ := cookiejar.New(nil)
	ghURL, _ := url.Parse(f.webBase())
	jar.SetCookies(ghURL, []*http.Cookie{
		{Name: "user_session", Value: f.ghUserSession},
		{Name: "__Host-user_session_same_site", Value: f.ghUserSession},
	})
	return &http.Client{Jar: jar, Timeout: 30 * time.Second}
}

// getRepoID fetches the numeric GitHub repository ID needed for the upload policy request.
func (f *ImageFixer) getRepoID() (int, error) {
	u := fmt.Sprintf("%s/repos/%s/%s", f.ghAPIBase, f.targetOrg, f.targetRepo)
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return 0, err
	}
	f.setGitHubAuth(req)

	resp, err := f.httpClient.Do(req)
	if err != nil {
		return 0, err
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("GET repo: HTTP %d", resp.StatusCode)
	}

	var repo struct {
		ID int `json:"id"`
	}
	if err := json.Unmarshal(body, &repo); err != nil {
		return 0, err
	}
	return repo.ID, nil
}

// getUploadToken GETs the repo page with the session client and extracts the
// uploadToken from the embedded JS payload. Requires write access to the repo.
func (f *ImageFixer) getUploadToken(client *http.Client) (string, error) {
	u := fmt.Sprintf("%s/%s/%s", f.webBase(), f.targetOrg, f.targetRepo)
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", ghUserAgent)

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("repo page returned %d — is the user_session cookie valid and does the account have write access?", resp.StatusCode)
	}

	m := uploadTokenPattern.FindSubmatch(body)
	if m == nil {
		return "", fmt.Errorf("uploadToken not found on repo page — account may not have write access to %s/%s", f.targetOrg, f.targetRepo)
	}
	return string(m[1]), nil
}

// requestUploadPolicy POSTs to GitHub's upload policy endpoint to obtain a
// pre-signed S3 URL and form fields for the image upload.
func (f *ImageFixer) requestUploadPolicy(client *http.Client, uploadToken string, repoID int, filename, contentType string, size int) (*uploadPolicyResponse, error) {
	body := &bytes.Buffer{}
	mw := multipart.NewWriter(body)
	for _, field := range []struct{ k, v string }{
		{"name", filename},
		{"size", strconv.Itoa(size)},
		{"content_type", contentType},
		{"authenticity_token", uploadToken},
		{"repository_id", strconv.Itoa(repoID)},
	} {
		if err := mw.WriteField(field.k, field.v); err != nil {
			return nil, err
		}
	}
	mw.Close()

	req, err := http.NewRequest("POST", f.webBase()+"/upload/policies/assets", body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Origin", f.webBase())
	req.Header.Set("Referer", fmt.Sprintf("%s/%s/%s", f.webBase(), f.targetOrg, f.targetRepo))
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("User-Agent", ghUserAgent)

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	respBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		return nil, fmt.Errorf("upload policy: HTTP %d: %s", resp.StatusCode, truncateMsg(string(respBody), 200))
	}

	var policy uploadPolicyResponse
	if err := json.Unmarshal(respBody, &policy); err != nil {
		return nil, fmt.Errorf("parsing upload policy: %w", err)
	}
	if policy.UploadURL == "" || policy.Asset.ID == 0 {
		return nil, fmt.Errorf("upload policy response missing required fields")
	}
	return &policy, nil
}

// s3FieldOrder defines the deterministic order S3 expects form fields in.
// S3 presigned POST uploads are sensitive to field ordering.
var s3FieldOrder = []string{
	"key",
	"acl",
	"policy",
	"X-Amz-Algorithm",
	"X-Amz-Credential",
	"X-Amz-Date",
	"X-Amz-Signature",
	"Content-Type",
	"Cache-Control",
	"x-amz-meta-Surrogate-Control",
}

// uploadToS3 uploads imgData to the S3 presigned URL from the upload policy.
// No GitHub auth is needed — the presigned form fields handle S3 authentication.
func (f *ImageFixer) uploadToS3(policy *uploadPolicyResponse, filename, contentType string, imgData []byte) error {
	body := &bytes.Buffer{}
	mw := multipart.NewWriter(body)

	// Write known fields in the required order first.
	written := make(map[string]bool)
	for _, key := range s3FieldOrder {
		val, ok := policy.Form[key]
		if !ok {
			continue
		}
		if err := mw.WriteField(key, val); err != nil {
			return err
		}
		written[key] = true
	}
	// Write any remaining fields not in the known order.
	for key, val := range policy.Form {
		if written[key] {
			continue
		}
		if err := mw.WriteField(key, val); err != nil {
			return err
		}
	}

	// File must be the last field.
	part, err := mw.CreateFormFile("file", filename)
	if err != nil {
		return err
	}
	if _, err := part.Write(imgData); err != nil {
		return err
	}
	mw.Close()

	req, err := http.NewRequest("POST", policy.UploadURL, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Origin", f.webBase())
	req.Header.Set("User-Agent", ghUserAgent)

	s3Client := &http.Client{Timeout: 120 * time.Second}
	resp, err := s3Client.Do(req)
	if err != nil {
		return err
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("S3 upload: HTTP %d", resp.StatusCode)
	}
	return nil
}

// finalizeUpload PUTs to /upload/assets/{id} to register the asset with GitHub.
// This step is mandatory — without it the user-attachments URL returns 404.
func (f *ImageFixer) finalizeUpload(client *http.Client, policy *uploadPolicyResponse) (string, error) {
	body := &bytes.Buffer{}
	mw := multipart.NewWriter(body)
	if err := mw.WriteField("authenticity_token", policy.AssetUploadAuthenticityToken); err != nil {
		return "", err
	}
	mw.Close()

	u := fmt.Sprintf("%s/upload/assets/%d", f.webBase(), policy.Asset.ID)
	req, err := http.NewRequest("PUT", u, body)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Origin", f.webBase())
	req.Header.Set("Referer", fmt.Sprintf("%s/%s/%s", f.webBase(), f.targetOrg, f.targetRepo))
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("User-Agent", ghUserAgent)

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	respBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("finalize upload: HTTP %d: %s", resp.StatusCode, truncateMsg(string(respBody), 200))
	}

	var result struct {
		Href string `json:"href"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", fmt.Errorf("parsing finalize response: %w", err)
	}
	if result.Href == "" {
		return "", fmt.Errorf("finalize response missing href")
	}
	return result.Href, nil
}

// webBase returns the GitHub web UI base URL derived from the API base URL.
// api.github.com → https://github.com
// https://ghe.example.com/api/v3 → https://ghe.example.com
func (f *ImageFixer) webBase() string {
	if strings.Contains(f.ghAPIBase, "api.github.com") {
		return "https://github.com"
	}
	return strings.TrimSuffix(strings.TrimSuffix(f.ghAPIBase, "/"), "/api/v3")
}

func (f *ImageFixer) updatePRBody(prNumber int, newBody string) error {
	u := fmt.Sprintf("%s/repos/%s/%s/pulls/%d",
		f.ghAPIBase, f.targetOrg, f.targetRepo, prNumber)

	payload, _ := json.Marshal(map[string]string{"body": newBody})
	req, err := http.NewRequest("PATCH", u, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	f.setGitHubAuth(req)
	req.Header.Set("Content-Type", "application/json")

	resp, err := f.httpClient.Do(req)
	if err != nil {
		return err
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("PATCH PR %d: HTTP %d", prNumber, resp.StatusCode)
	}
	return nil
}

func (f *ImageFixer) setGitHubAuth(req *http.Request) {
	req.Header.Set("Authorization", "Bearer "+f.ghToken)
}

func extractFilename(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return "image.png"
	}
	parts := strings.Split(parsed.Path, "/")
	for i := len(parts) - 1; i >= 0; i-- {
		if parts[i] != "" {
			name, _ := url.PathUnescape(parts[i])
			return name
		}
	}
	return "image.png"
}

func contentTypeFromFilename(name string) string {
	lower := strings.ToLower(name)
	ext := lower[strings.LastIndex(lower, "."):]
	ct := mime.TypeByExtension(ext)
	if ct != "" {
		return ct
	}
	switch ext {
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".png":
		return "image/png"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".svg":
		return "image/svg+xml"
	default:
		return "image/png"
	}
}

func truncateMsg(s string, maxLen int) string {
	r := []rune(s)
	if len(r) <= maxLen {
		return s
	}
	return string(r[:maxLen]) + "..."
}
