package hermes

import (
	"context"
	"fmt"
	"io"
	"log"

	http "github.com/bogdanfinn/fhttp"
)

// bootstrapSession warms a fresh client: load the homepage, solve any DataDome
// challenge that is present, post the DataDome tags (ch + le, i.e. twice) like a
// browser's first page load, then call /sync-form to mint the x-xsrf-token (and
// the rest of the ecom session cookies). After this the session cookies are
// valid for the bck.hermes.com JSON API.
func bootstrapSession(ctx context.Context, c *wrappedClient, workerID int, locale string) error {
	if c.bootstrapped {
		return nil
	}

	if err := loadHomepage(ctx, c, workerID); err != nil {
		return fmt.Errorf("load homepage: %w", err)
	}

	// Post tags twice (ch then le) on first page load, matching the browser.
	if err := solveDataDomeTags(ctx, c, HomepageURL); err != nil {
		return fmt.Errorf("post DataDome tags on bootstrap: %w", err)
	}
	scrapeLogf(workerID, "session bootstrapped: homepage warmed and datadome tags posted")

	// /sync-form returns the x-xsrf-token cookie that API calls echo back as a header.
	if err := syncForm(ctx, c, locale, workerID); err != nil {
		return fmt.Errorf("sync form: %w", err)
	}

	c.bootstrapped = true
	c.apiCalls = 0
	return nil
}

func loadHomepage(ctx context.Context, c *wrappedClient, workerID int) error {
	doRequest := func() (*http.Response, error) {
		req, _ := http.NewRequestWithContext(ctx, "GET", HomepageURL, nil)
		req.Header = homepageHeaders(c)
		return c.Client.Do(req)
	}

	resp, err := doRequest()
	if err != nil {
		return err
	}

	if resp.StatusCode == 403 {
		scrapeLogf(workerID, "datadome 403 on homepage; solving challenge")

		blockedBody, readErr := io.ReadAll(resp.Body)
		if closeErr := resp.Body.Close(); closeErr != nil && readErr == nil {
			readErr = closeErr
		}
		if readErr != nil {
			return readErr
		}

		if err := solveDataDome(ctx, c, HomepageURL, blockedBody); err != nil {
			return fmt.Errorf("solve DataDome on homepage: %w", err)
		}

		resp, err = doRequest()
		if err != nil {
			return err
		}
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		_, _ = io.Copy(io.Discard, resp.Body)
		return fmt.Errorf("homepage returned status %d", resp.StatusCode)
	}
	_, _ = io.Copy(io.Discard, resp.Body)

	c.isFirstRequest = false
	return nil
}

// syncForm calls bck.hermes.com/sync-form, which sets the x-xsrf-token cookie
// (and the ecom session cookies) used by subsequent API calls.
func syncForm(ctx context.Context, c *wrappedClient, locale string, workerID int) error {
	syncURL := SyncFormURL(locale)
	if _, err := apiRequestBody(ctx, c, syncURL, workerID); err != nil {
		return err
	}
	if getCookieValue(c, syncURL, "x-xsrf-token") == "" {
		scrapeLogf(workerID, "warning: sync-form did not set x-xsrf-token cookie")
	}
	return nil
}

// apiGet fetches a bck.hermes.com JSON endpoint using the warmed session. The
// session is reused until it errors or hard-blocks; there is no voluntary
// rotation. apiCalls is tracked only for logging how long a session lasted.
func apiGet(ctx context.Context, c *wrappedClient, apiURL string, workerID int) ([]byte, error) {
	body, err := apiRequestBody(ctx, c, apiURL, workerID)
	if err != nil {
		return nil, err
	}

	c.apiCalls++
	return body, nil
}

// apiRequestBody performs a GET against a bck.hermes.com endpoint with the JSON
// API headers. It handles both DataDome challenge forms — the XHR JSON
// {"url":"https://geo.captcha-delivery.com/..."} and the classic 403 HTML block
// carrying a var dd={...} object — by solving, re-posting tags, and retrying once.
func apiRequestBody(ctx context.Context, c *wrappedClient, apiURL string, workerID int) ([]byte, error) {
	doRequest := func() (*http.Response, error) {
		req, _ := http.NewRequestWithContext(ctx, "GET", apiURL, nil)
		req.Header = apiHeaders(c, apiURL)
		return c.Client.Do(req)
	}

	const maxAttempts = 2
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		resp, err := doRequest()
		if err != nil {
			return nil, err
		}

		body, err := readResponseBody(resp)
		if err != nil {
			return nil, err
		}

		// DataDome XHR challenge: a JSON body pointing at a captcha-delivery device link.
		if deviceLink, ok := datadomeChallengeDeviceLink(body); ok {
			if attempt == maxAttempts {
				return nil, fmt.Errorf("api GET %s still challenged after solve", apiURL)
			}
			scrapeLogf(workerID, "datadome challenge on api call url=%s", apiURL)

			if err := solveDataDomeChallengeURL(ctx, c, SiteReferer, deviceLink); err != nil {
				return nil, fmt.Errorf("solve DataDome challenge on api call: %w", err)
			}
			//if err := solveDataDomeTags(ctx, c, HomepageURL); err != nil {
			//	return nil, fmt.Errorf("post DataDome tags after api challenge: %w", err)
			//}
			continue
		}

		// Classic 403 HTML block with a var dd={...} object.
		if resp.StatusCode == 403 {
			if attempt == maxAttempts {
				return nil, fmt.Errorf("api GET %s still blocked after solve", apiURL)
			}
			scrapeLogf(workerID, "datadome 403 on api call url=%s", apiURL)

			if err := solveDataDome(ctx, c, SiteReferer, body); err != nil {
				return nil, fmt.Errorf("solve DataDome on api call: %w", err)
			}
			if err := solveDataDomeTags(ctx, c, HomepageURL); err != nil {
				return nil, fmt.Errorf("post DataDome tags after api solve: %w", err)
			}
			continue
		}

		if resp.StatusCode != 200 {
			return nil, fmt.Errorf("api GET %s returned status %d: %s", apiURL, resp.StatusCode, responseSnippet(body))
		}

		return body, nil
	}

	return nil, fmt.Errorf("api GET %s exhausted DataDome retries", apiURL)
}

func readResponseBody(resp *http.Response) ([]byte, error) {
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

func homepageHeaders(c *wrappedClient) http.Header {
	return http.Header{
		"sec-ch-ua":                 {ClientHints},
		"sec-ch-ua-mobile":          {"?0"},
		"sec-ch-ua-platform":        {`"Windows"`},
		"upgrade-insecure-requests": {"1"},
		"user-agent":                {UserAgent},
		"accept":                    {"text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8,application/signed-exchange;v=b3;q=0.7"},
		"sec-fetch-site":            {"none"},
		"sec-fetch-mode":            {"navigate"},
		"sec-fetch-user":            {"?1"},
		"sec-fetch-dest":            {"document"},
		"accept-encoding":           {"gzip, deflate, br, zstd"},
		"accept-language":           {c.AcceptLanguage},
		"priority":                  {"u=0, i"},
		http.HeaderOrderKey:         {"sec-ch-ua", "sec-ch-ua-mobile", "sec-ch-ua-platform", "upgrade-insecure-requests", "user-agent", "accept", "sec-fetch-site", "sec-fetch-mode", "sec-fetch-user", "sec-fetch-dest", "accept-encoding", "accept-language", "priority"},
	}
}

func apiHeaders(c *wrappedClient, apiURL string) http.Header {
	header := http.Header{
		"sec-ch-ua-platform": {`"Windows"`},
		"user-agent":         {UserAgent},
		"accept":             {"application/json, text/plain, */*"},
		"sec-ch-ua":          {ClientHints},
		"sec-ch-ua-mobile":   {"?0"},
		"origin":             {SiteOrigin},
		"sec-fetch-site":     {"same-site"},
		"sec-fetch-mode":     {"cors"},
		"sec-fetch-dest":     {"empty"},
		"referer":            {SiteReferer},
		"accept-encoding":    {"gzip, deflate, br, zstd"},
		"accept-language":    {c.AcceptLanguage},
		"priority":           {"u=1, i"},
	}

	order := []string{"sec-ch-ua-platform"}

	xsrf := getCookieValue(c, apiURL, "x-xsrf-token")
	if xsrf == "" {
		xsrf = getCookieValue(c, HomepageURL, "x-xsrf-token")
	}
	if xsrf != "" {
		header["x-xsrf-token"] = []string{xsrf}
		order = append(order, "x-xsrf-token")
	}

	order = append(order,
		"user-agent", "accept", "sec-ch-ua", "sec-ch-ua-mobile", "origin",
		"sec-fetch-site", "sec-fetch-mode", "sec-fetch-dest", "referer",
		"accept-encoding", "accept-language", "cookie", "priority",
	)

	header[http.HeaderOrderKey] = order
	return header
}

func scrapeLogf(workerID int, format string, args ...any) {
	if workerID > 0 {
		log.Printf("worker=%02d "+format, append([]any{workerID}, args...)...)
		return
	}

	log.Printf(format, args...)
}
