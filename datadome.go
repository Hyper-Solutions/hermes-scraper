package hermes

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	hyper "github.com/Hyper-Solutions/hyper-sdk-go/v2"
	http "github.com/bogdanfinn/fhttp"
)

const (
	datadomeHermesDDK             = "2211F522B61E269B869FA6EAFFB5E1"
	datadomeInterstitialEndpoint  = "https://geo.captcha-delivery.com/interstitial/"
	datadomeTagsEndpoint          = "https://dd.hermes.com/js/"
	datadomeTagsVersion           = "5.7.0"
	datadomeTagsInitialCookieName = ".keep"

	// datadomeTagsSettleDelay is how long to wait after posting the tags before
	// using the session, mirroring the pause a real browser has between page
	// load and the next action.
	datadomeTagsSettleDelay = 10 * time.Second
)

var (
	captchaChallengePathRe = regexp.MustCompile(`captchaChallengePath:\s*['"]([^'"]+)['"]`)
	datadomeCookieBodyRe   = regexp.MustCompile(`(?i)(?:datadome=|cookie['"]?\s*:\s*['"])([^;'"\s]+)`)
	ddObjectRe             = regexp.MustCompile(`var\s+dd\s*=\s*\{\s*([\s\S]*?)\s*}`)
	singleQuoteRe          = regexp.MustCompile(`'([^']*)'`)
	unquotedKeyRe          = regexp.MustCompile(`([^"]|^)(\b\w+)\s*:`)
)

type datadomeBlock struct {
	Rt  string `json:"rt"`
	Cid string `json:"cid"`
	Hsh string `json:"hsh"`
	B   int64  `json:"b"`
	S   int64  `json:"s"`
	E   string `json:"e"`
	T   string `json:"t"`
}

type datadomeInterstitialResponse struct {
	Cookie string `json:"cookie"`
	View   string `json:"view"`
	URL    string `json:"url"`
}

type datadomeSliderResponse struct {
	Cookie string `json:"cookie"`
}

type datadomeTagsResponse struct {
	Cookie string `json:"cookie"`
	URL    string `json:"url"`
}

// datadomeChallengeResponse is the XHR challenge form DataDome returns to API
// requests: a JSON body whose url points at a ready-made geo.captcha-delivery.com
// device link, instead of the HTML block page carrying a var dd={...} object.
type datadomeChallengeResponse struct {
	URL string `json:"url"`
}

func solveDataDome(ctx context.Context, c *wrappedClient, pageURL string, blockedBody []byte) error {
	if HyperSdk == nil || HyperSdk.ApiKey == "" {
		return errors.New("API_KEY is required to solve DataDome")
	}

	datadomeCookie := getCookieValue(c, pageURL, "datadome")
	if datadomeCookie == "" {
		datadomeCookie = extractDatadomeCookie(blockedBody)
	}

	deviceLink, isInterstitial, err := parseDatadomeDeviceLink(blockedBody, datadomeCookie, pageURL)
	if err != nil {
		return err
	}

	var headers *hyper.Headers
	if isInterstitial {
		headers, err = solveDataDomeInterstitial(ctx, c, pageURL, deviceLink)
	} else {
		headers, err = solveDataDomeSlider(ctx, c, pageURL, deviceLink)
	}
	if err != nil {
		return err
	}
	if headers == nil {
		return errors.New("DataDome solve did not return client-hint headers")
	}

	c.headers = headers
	c.isFirstRequest = false
	return nil
}

// datadomeChallengeDeviceLink reports whether a response body is a DataDome XHR
// challenge (a JSON {"url":"https://geo.captcha-delivery.com/..."}), returning
// the device link if so.
func datadomeChallengeDeviceLink(body []byte) (string, bool) {
	if !bytes.Contains(body, []byte("captcha-delivery.com")) {
		return "", false
	}

	var response datadomeChallengeResponse
	if err := json.Unmarshal(bytes.TrimSpace(body), &response); err != nil {
		return "", false
	}
	if response.URL == "" || !strings.Contains(response.URL, "captcha-delivery.com") {
		return "", false
	}
	return response.URL, true
}

// solveDataDomeChallengeURL solves a DataDome challenge whose device link was
// delivered directly (interstitial or slider, chosen by the link path).
func solveDataDomeChallengeURL(ctx context.Context, c *wrappedClient, pageURL string, deviceLink string) error {
	if HyperSdk == nil || HyperSdk.ApiKey == "" {
		return errors.New("API_KEY is required to solve DataDome")
	}

	var (
		headers *hyper.Headers
		err     error
	)
	if strings.Contains(deviceLink, "/interstitial/") {
		headers, err = solveDataDomeInterstitial(ctx, c, pageURL, deviceLink)
	} else {
		headers, err = solveDataDomeSlider(ctx, c, pageURL, deviceLink)
	}
	if err != nil {
		return err
	}
	if headers != nil {
		c.headers = headers
	}
	c.isFirstRequest = false
	return nil
}

func parseDatadomeDeviceLink(blockedBody []byte, datadomeCookie string, pageURL string) (string, bool, error) {
	block, err := parseDatadomeBlock(blockedBody)
	if err != nil {
		return "", false, err
	}
	if datadomeCookie == "" {
		datadomeCookie = block.Cid
	}

	if bytes.Contains(blockedBody, []byte("https://ct.captcha-delivery.com/i.js")) || block.Rt == "i" {
		return buildDatadomeInterstitialLink(block, datadomeCookie, pageURL), true, nil
	}

	if block.T == "bv" {
		return "", false, errors.New("DataDome returned blocked-proxy challenge")
	}
	return buildDatadomeSliderLink(block, datadomeCookie, pageURL), false, nil
}

func solveDataDomeTags(ctx context.Context, c *wrappedClient, pageURL string) error {
	if HyperSdk == nil || HyperSdk.ApiKey == "" {
		return errors.New("API_KEY is required to generate DataDome tags")
	}

	if err := solveDataDomeTag(ctx, c, pageURL, datadomeTagsInitialCookieName, "ch"); err != nil {
		return fmt.Errorf("post DataDome ch tags: %w", err)
	}

	cid := getCookieValue(c, pageURL, "datadome")
	if cid == "" {
		return errors.New("DataDome ch tags did not set datadome cookie")
	}

	if err := solveDataDomeTag(ctx, c, pageURL, cid, "le"); err != nil {
		return fmt.Errorf("post DataDome le tags: %w", err)
	}

	return sleepWithContext(ctx, datadomeTagsSettleDelay)
}

// sleepWithContext waits for d, returning early if the context is canceled.
func sleepWithContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func solveDataDomeTag(ctx context.Context, c *wrappedClient, pageURL string, cid string, tagType string) error {
	recordDataDomeTagsCall(c)

	payload, err := HyperSdk.GenerateDataDomeTags(ctx, &hyper.DataDomeTagsInput{
		UserAgent:      UserAgent,
		Cid:            cid,
		Ddk:            datadomeHermesDDK,
		Referer:        pageURL,
		Type:           tagType,
		Version:        datadomeTagsVersion,
		AcceptLanguage: c.AcceptLanguage,
		IP:             c.Ip,
	})
	if err != nil {
		return err
	}

	return postDataDomeTags(ctx, c, pageURL, payload)
}

func solveDataDomeInterstitial(ctx context.Context, c *wrappedClient, pageURL string, deviceLink string) (*hyper.Headers, error) {
	html, err := getDataDomeFrame(ctx, c, deviceLink, pageURL)
	if err != nil {
		return nil, err
	}

	recordDataDomeInterstitialCall(c)
	payload, headers, err := HyperSdk.GenerateDataDomeInterstitial(ctx, &hyper.DataDomeInterstitialInput{
		UserAgent:      UserAgent,
		DeviceLink:     deviceLink,
		Html:           string(html),
		AcceptLanguage: c.AcceptLanguage,
		IP:             c.Ip,
	})
	if err != nil {
		return nil, err
	}

	responseBody, err := postDataDomeInterstitial(ctx, c, deviceLink, payload)
	if err != nil {
		return nil, err
	}

	var response datadomeInterstitialResponse
	if err := json.Unmarshal(responseBody, &response); err != nil {
		return nil, fmt.Errorf("decode DataDome interstitial response: %w", err)
	}
	if response.Cookie != "" {
		if err := setCookieHeader(c, pageURL, response.Cookie); err != nil {
			return nil, err
		}
	}

	switch response.View {
	case "redirect":
		return headers, nil
	case "captcha":
		if response.URL == "" {
			return nil, errors.New("DataDome interstitial requested captcha without URL")
		}
		return solveDataDomeSlider(ctx, c, pageURL, response.URL)
	default:
		if response.Cookie != "" {
			return headers, nil
		}
		return nil, fmt.Errorf("unexpected DataDome interstitial view %q", response.View)
	}
}

func solveDataDomeSlider(ctx context.Context, c *wrappedClient, pageURL string, deviceLink string) (*hyper.Headers, error) {
	if isBlockedProxyDeviceLink(deviceLink) {
		return nil, errors.New("DataDome returned blocked-proxy challenge")
	}

	html, err := getDataDomeFrame(ctx, c, deviceLink, pageURL)
	if err != nil {
		return nil, err
	}

	puzzleURL, err := parseCaptchaChallengePath(string(html), deviceLink)
	if err != nil {
		return nil, err
	}
	pieceURL, err := puzzlePieceURL(puzzleURL)
	if err != nil {
		return nil, err
	}

	piece, err := getDataDomeImage(ctx, c, pieceURL)
	if err != nil {
		return nil, fmt.Errorf("get DataDome puzzle piece: %w", err)
	}
	puzzle, err := getDataDomeImage(ctx, c, puzzleURL)
	if err != nil {
		return nil, fmt.Errorf("get DataDome puzzle: %w", err)
	}

	recordDataDomeSliderCall(c)
	checkURL, headers, err := HyperSdk.GenerateDataDomeSlider(ctx, &hyper.DataDomeSliderInput{
		UserAgent:      UserAgent,
		DeviceLink:     deviceLink,
		Html:           string(html),
		Puzzle:         base64.StdEncoding.EncodeToString(puzzle),
		Piece:          base64.StdEncoding.EncodeToString(piece),
		ParentUrl:      pageURL,
		AcceptLanguage: c.AcceptLanguage,
		IP:             c.Ip,
	})
	if err != nil {
		return nil, err
	}

	cookie, err := getDataDomeSliderCookie(ctx, c, checkURL, deviceLink)
	if err != nil {
		return nil, err
	}
	if cookie == "" {
		return nil, errors.New("DataDome slider response did not include a cookie")
	}
	if err := setCookieHeader(c, pageURL, cookie); err != nil {
		return nil, err
	}

	return headers, nil
}

func getDataDomeFrame(ctx context.Context, c *wrappedClient, frameURL string, referer string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", frameURL, nil)
	if err != nil {
		return nil, err
	}

	// geo.captcha-delivery.com serves HTTP/1.1, where Chrome sends Title-Case
	// header names (only sec-ch-ua* stay lowercase). Casing is on the wire on h1.1.
	req.Header = http.Header{
		"Connection":                {"keep-alive"},
		"sec-ch-ua":                 {ClientHints},
		"sec-ch-ua-mobile":          {"?0"},
		"sec-ch-ua-platform":        {`"Windows"`},
		"Upgrade-Insecure-Requests": {"1"},
		"User-Agent":                {UserAgent},
		"Accept":                    {"text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8,application/signed-exchange;v=b3;q=0.7"},
		"Sec-Fetch-Site":            {"cross-site"},
		"Sec-Fetch-Mode":            {"navigate"},
		"Sec-Fetch-Dest":            {"iframe"},
		"Sec-Fetch-Storage-Access":  {"active"},
		"Referer":                   {referer},
		"Accept-Encoding":           {"gzip, deflate, br, zstd"},
		"Accept-Language":           {c.AcceptLanguage},
		http.HeaderOrderKey:         {"Host", "Connection", "sec-ch-ua", "sec-ch-ua-mobile", "sec-ch-ua-platform", "Upgrade-Insecure-Requests", "User-Agent", "Accept", "Sec-Fetch-Site", "Sec-Fetch-Mode", "Sec-Fetch-Dest", "Sec-Fetch-Storage-Access", "Referer", "Accept-Encoding", "Accept-Language"},
	}

	resp, err := c.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("GET DataDome frame returned %d: %s", resp.StatusCode, responseSnippet(body))
	}

	return body, nil
}

func postDataDomeTags(ctx context.Context, c *wrappedClient, pageURL string, payload string) error {
	origin, err := originForURL(pageURL)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", datadomeTagsEndpoint, strings.NewReader(payload))
	if err != nil {
		return err
	}

	// dd.hermes.com serves HTTP/2 (lowercase header names). Order and values
	// match the browser's tags POST: same-site, referer at the site root.
	req.Header = http.Header{
		"sec-ch-ua-platform": {`"Windows"`},
		"user-agent":         {UserAgent},
		"sec-ch-ua":          {ClientHints},
		"content-type":       {"application/x-www-form-urlencoded"},
		"sec-ch-ua-mobile":   {"?0"},
		"accept":             {"*/*"},
		"origin":             {origin},
		"sec-fetch-site":     {"same-site"},
		"sec-fetch-mode":     {"cors"},
		"sec-fetch-dest":     {"empty"},
		"referer":            {SiteReferer},
		"accept-encoding":    {"gzip, deflate, br, zstd"},
		"accept-language":    {c.AcceptLanguage},
		"priority":           {"u=1, i"},
		http.HeaderOrderKey:  {"content-length", "sec-ch-ua-platform", "user-agent", "sec-ch-ua", "content-type", "sec-ch-ua-mobile", "accept", "origin", "sec-fetch-site", "sec-fetch-mode", "sec-fetch-dest", "referer", "accept-encoding", "accept-language", "cookie", "priority"},
		http.PHeaderOrderKey: {":method", ":authority", ":scheme", ":path"},
	}

	// The tags request must be sent with no cookies. Swap in a nil jar for the
	// request so the client attaches none, then restore the original jar.
	oldJar := c.Client.GetCookieJar()
	c.Client.SetCookieJar(nil)
	resp, err := c.Client.Do(req)
	c.Client.SetCookieJar(oldJar)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("POST DataDome tags returned %d: %s", resp.StatusCode, responseSnippet(body))
	}

	var response datadomeTagsResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return fmt.Errorf("decode DataDome tags response: %w", err)
	}
	if response.Cookie == "" {
		return errors.New("DataDome tags response did not include a cookie")
	}

	// Store the datadome cookie the response returned in the restored jar.
	return setCookieHeader(c, pageURL, response.Cookie)
}

func postDataDomeInterstitial(ctx context.Context, c *wrappedClient, deviceLink string, payload string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", datadomeInterstitialEndpoint, strings.NewReader(payload))
	if err != nil {
		return nil, err
	}

	// geo.captcha-delivery.com serves HTTP/1.1 (Chrome Title-Case header names).
	req.Header = http.Header{
		"Connection":               {"keep-alive"},
		"sec-ch-ua-platform":       {`"Windows"`},
		"User-Agent":               {UserAgent},
		"sec-ch-ua":                {ClientHints},
		"Content-Type":             {"application/x-www-form-urlencoded; charset=UTF-8"},
		"sec-ch-ua-mobile":         {"?0"},
		"Accept":                   {"*/*"},
		"Origin":                   {"https://geo.captcha-delivery.com"},
		"Sec-Fetch-Site":           {"same-origin"},
		"Sec-Fetch-Mode":           {"cors"},
		"Sec-Fetch-Dest":           {"empty"},
		"Sec-Fetch-Storage-Access": {"active"},
		"Referer":                  {deviceLink},
		"Accept-Encoding":          {"gzip, deflate, br, zstd"},
		"Accept-Language":          {c.AcceptLanguage},
		http.HeaderOrderKey:        {"Host", "Connection", "Content-Length", "sec-ch-ua-platform", "User-Agent", "sec-ch-ua", "Content-Type", "sec-ch-ua-mobile", "Accept", "Origin", "Sec-Fetch-Site", "Sec-Fetch-Mode", "Sec-Fetch-Dest", "Sec-Fetch-Storage-Access", "Referer", "Accept-Encoding", "Accept-Language"},
	}

	resp, err := c.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("POST DataDome interstitial returned %d: %s", resp.StatusCode, responseSnippet(body))
	}

	return body, nil
}

func getDataDomeImage(ctx context.Context, c *wrappedClient, imageURL string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", imageURL, nil)
	if err != nil {
		return nil, err
	}

	req.Header = http.Header{
		"origin":             {"https://geo.captcha-delivery.com"},
		"sec-ch-ua-platform": {`"Windows"`},
		"user-agent":         {UserAgent},
		"sec-ch-ua":          {ClientHints},
		"sec-ch-ua-mobile":   {"?0"},
		"accept":             {"image/avif,image/webp,image/apng,image/svg+xml,image/*,*/*;q=0.8"},
		"sec-fetch-site":     {"same-site"},
		"sec-fetch-mode":     {"cors"},
		"sec-fetch-dest":     {"image"},
		"referer":            {"https://geo.captcha-delivery.com/"},
		"accept-encoding":    {"gzip, deflate, br, zstd"},
		"accept-language":    {c.AcceptLanguage},
		"priority":           {"i"},
		http.HeaderOrderKey:  {"origin", "sec-ch-ua-platform", "user-agent", "sec-ch-ua", "sec-ch-ua-mobile", "accept", "sec-fetch-site", "sec-fetch-mode", "sec-fetch-dest", "referer", "accept-encoding", "accept-language", "cookie", "priority"},
	}

	resp, err := c.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("GET DataDome image returned %d: %s", resp.StatusCode, responseSnippet(body))
	}

	return body, nil
}

func getDataDomeSliderCookie(ctx context.Context, c *wrappedClient, checkURL string, referer string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", checkURL, nil)
	if err != nil {
		return "", err
	}

	// geo.captcha-delivery.com serves HTTP/1.1 (Chrome Title-Case header names).
	req.Header = http.Header{
		"Connection":               {"keep-alive"},
		"sec-ch-ua-platform":       {`"Windows"`},
		"User-Agent":               {UserAgent},
		"sec-ch-ua":                {ClientHints},
		"Content-Type":             {"application/x-www-form-urlencoded; charset=UTF-8"},
		"sec-ch-ua-mobile":         {"?0"},
		"Accept":                   {"*/*"},
		"Sec-Fetch-Site":           {"same-origin"},
		"Sec-Fetch-Mode":           {"cors"},
		"Sec-Fetch-Dest":           {"empty"},
		"Sec-Fetch-Storage-Access": {"active"},
		"Referer":                  {referer},
		"Accept-Encoding":          {"gzip, deflate, br, zstd"},
		"Accept-Language":          {c.AcceptLanguage},
		http.HeaderOrderKey:        {"Host", "Connection", "sec-ch-ua-platform", "User-Agent", "sec-ch-ua", "Content-Type", "sec-ch-ua-mobile", "Accept", "Sec-Fetch-Site", "Sec-Fetch-Mode", "Sec-Fetch-Dest", "Sec-Fetch-Storage-Access", "Referer", "Accept-Encoding", "Accept-Language"},
	}

	resp, err := c.Client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode == 403 {
		return "", errors.New("DataDome slider check was blocked")
	}
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("GET DataDome slider check returned %d: %s", resp.StatusCode, responseSnippet(body))
	}

	var response datadomeSliderResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return "", fmt.Errorf("decode DataDome slider response: %w", err)
	}

	return response.Cookie, nil
}

func parseCaptchaChallengePath(html string, deviceLink string) (string, error) {
	matches := captchaChallengePathRe.FindStringSubmatch(html)
	if len(matches) < 2 {
		return "", errors.New("DataDome captcha challenge path not found")
	}

	return resolveURL(deviceLink, matches[1])
}

func parseDatadomeBlock(blockedBody []byte) (*datadomeBlock, error) {
	matches := ddObjectRe.FindSubmatch(blockedBody)
	if len(matches) < 2 {
		return nil, errors.New("DataDome dd object not found")
	}

	jsonObject := append([]byte("{"), bytes.TrimSpace(matches[1])...)
	jsonObject = append(jsonObject, '}')
	jsonObject = singleQuoteRe.ReplaceAll(jsonObject, []byte(`"$1"`))
	jsonObject = unquotedKeyRe.ReplaceAll(jsonObject, []byte(`$1"$2":`))

	var block datadomeBlock
	if err := json.Unmarshal(jsonObject, &block); err != nil {
		return nil, fmt.Errorf("decode DataDome dd object: %w", err)
	}

	return &block, nil
}

func buildDatadomeInterstitialLink(block *datadomeBlock, datadomeCookie string, pageURL string) string {
	pairs := []queryPair{
		{key: "initialCid", value: block.Cid},
		{key: "hash", value: block.Hsh},
		{key: "cid", value: datadomeCookie},
		{key: "referer", value: pageURL},
		{key: "s", value: strconv.FormatInt(block.S, 10)},
	}
	if block.E != "" {
		pairs = append(pairs, queryPair{key: "e", value: block.E})
	}
	pairs = append(pairs,
		queryPair{key: "b", value: strconv.FormatInt(block.B, 10)},
		queryPair{key: "dm", value: "cd"},
	)

	return "https://geo.captcha-delivery.com/interstitial/?" + encodeOrderedQuery(pairs)
}

func buildDatadomeSliderLink(block *datadomeBlock, datadomeCookie string, pageURL string) string {
	pairs := []queryPair{
		{key: "initialCid", value: block.Cid},
		{key: "hash", value: block.Hsh},
		{key: "cid", value: datadomeCookie},
		{key: "t", value: block.T},
		{key: "referer", value: pageURL},
		{key: "s", value: strconv.FormatInt(block.S, 10)},
		{key: "e", value: block.E},
		{key: "dm", value: "cd"},
	}

	return "https://geo.captcha-delivery.com/captcha/?" + encodeOrderedQuery(pairs)
}

type queryPair struct {
	key   string
	value string
}

func encodeOrderedQuery(pairs []queryPair) string {
	var builder strings.Builder
	for index, pair := range pairs {
		if index > 0 {
			builder.WriteByte('&')
		}
		builder.WriteString(url.QueryEscape(pair.key))
		builder.WriteByte('=')
		builder.WriteString(url.QueryEscape(pair.value))
	}
	return builder.String()
}

func puzzlePieceURL(puzzleURL string) (string, error) {
	if !strings.Contains(puzzleURL, ".jpg") {
		return "", fmt.Errorf("unexpected DataDome puzzle URL %q", puzzleURL)
	}
	return strings.Replace(puzzleURL, ".jpg", ".frag.png", 1), nil
}

func resolveURL(base string, raw string) (string, error) {
	if strings.HasPrefix(raw, "//") {
		return "https:" + raw, nil
	}

	parsedRaw, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	if parsedRaw.IsAbs() {
		return parsedRaw.String(), nil
	}

	parsedBase, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	return parsedBase.ResolveReference(parsedRaw).String(), nil
}

func originForURL(rawURL string) (string, error) {
	parsedURL, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	if parsedURL.Scheme == "" || parsedURL.Host == "" {
		return "", fmt.Errorf("URL has no origin: %s", rawURL)
	}
	return parsedURL.Scheme + "://" + parsedURL.Host, nil
}

func getCookieValue(c *wrappedClient, rawURL string, name string) string {
	parsedURL, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}

	for _, cookie := range c.Client.GetCookies(parsedURL) {
		if cookie.Name == name {
			return cookie.Value
		}
	}

	return ""
}

func extractDatadomeCookie(body []byte) string {
	matches := datadomeCookieBodyRe.FindSubmatch(body)
	if len(matches) < 2 {
		return ""
	}
	return string(matches[1])
}

func setCookieHeader(c *wrappedClient, rawURL string, setCookie string) error {
	parsedURL, err := url.Parse(rawURL)
	if err != nil {
		return err
	}

	header := http.Header{}
	header.Add("Set-Cookie", setCookie)
	response := http.Response{Header: header}
	cookies := response.Cookies()
	if len(cookies) == 0 {
		return errors.New("DataDome response cookie could not be parsed")
	}

	c.Client.SetCookies(parsedURL, cookies)
	return nil
}

func isBlockedProxyDeviceLink(deviceLink string) bool {
	parsedURL, err := url.Parse(deviceLink)
	if err != nil {
		return false
	}
	return parsedURL.Query().Get("t") == "bv"
}

func responseSnippet(body []byte) string {
	const maxSnippetLength = 300
	snippet := strings.TrimSpace(string(body))
	if len(snippet) > maxSnippetLength {
		snippet = snippet[:maxSnippetLength] + "..."
	}
	return snippet
}
