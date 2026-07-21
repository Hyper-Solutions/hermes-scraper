// Command skus walks Hermès category listings on the bck.hermes.com products
// API, paginating each category until it is exhausted, and writes the collected
// products (SKUs plus basic fields) to a JSON file for the product scraper.
//
// Pass -locale all to crawl every market (hermes.Locales); each market is written
// to its own file, skus-<locale>.json.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"hermes"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	progressInterval        = 30 * time.Second
	maxCategoryPageAttempts = 4
)

type catalogEntry struct {
	Category string `json:"category"`
	hermes.ListedProduct
}

type catalog struct {
	Locale     string         `json:"locale"`
	Sort       string         `json:"sort"`
	PageSize   int            `json:"pagesize"`
	Categories []string       `json:"categories"`
	Count      int            `json:"count"`
	Products   []catalogEntry `json:"products"`
}

func main() {
	categoriesFlag := flag.String("categories", "auto", `Comma-separated category codes, "top" to discover top-level categories from /menu, or "auto" for every category (top-level already aggregates its sub-tree)`)
	sortOrder := flag.String("sort", "relevance", "Sort order query parameter")
	pageSize := flag.Int("pagesize", 144, "Page size per request")
	localeFlag := flag.String("locale", "all", `Locale query parameter, e.g. us_en, or "all" for every market`)
	maxOffset := flag.Int("max-offset", 100000, "Safety cap on offset per category")
	threads := flag.Int("threads", 100, "Number of categories crawled concurrently")
	outPath := flag.String("out", "skus.json", "JSON output path (a -<locale> suffix is added per market when -locale all)")
	flag.Parse()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	interrupts := make(chan os.Signal, 1)
	signal.Notify(interrupts, os.Interrupt)
	defer signal.Stop(interrupts)
	go func() {
		<-interrupts
		log.Printf("received interrupt; canceling")
		cancel()
	}()

	locales := localesFor(*localeFlag)
	multi := len(locales) > 1
	startedAll := time.Now()

	for _, loc := range locales {
		if ctx.Err() != nil {
			break
		}
		out := *outPath
		if multi {
			out = withLocaleSuffix(*outPath, loc)
		}
		if err := crawlLocale(ctx, loc, *categoriesFlag, *sortOrder, *pageSize, *maxOffset, *threads, out); err != nil {
			log.Printf("[%s] %v", loc, err)
		}
	}

	telemetry := hermes.GetTelemetrySnapshot()
	log.Printf(
		"all done: %d locale(s) in %s; total bandwidth %s, datadome calls interstitial=%d slider=%d tags=%d",
		len(locales), time.Since(startedAll).Round(time.Second),
		hermes.FormatBandwidth(telemetry.BandwidthBytes),
		telemetry.DataDomeInterstitialCalls, telemetry.DataDomeSliderCalls, telemetry.DataDomeTagsCalls,
	)
}

func localesFor(value string) []string {
	if strings.EqualFold(strings.TrimSpace(value), "all") {
		return hermes.Locales
	}
	return []string{value}
}

func withLocaleSuffix(path, locale string) string {
	ext := filepath.Ext(path)
	return strings.TrimSuffix(path, ext) + "-" + locale + ext
}

func crawlLocale(ctx context.Context, locale, categoriesFlag, sortOrder string, pageSize, maxOffset, threads int, outPath string) error {
	categories, err := resolveCategories(ctx, categoriesFlag, locale)
	if err != nil {
		return fmt.Errorf("resolve categories: %w", err)
	}

	startedAt := time.Now()
	log.Printf("[%s] crawling %d categories with pagesize %d", locale, len(categories), pageSize)

	var (
		mu               sync.Mutex
		entries          []catalogEntry
		seen             = map[string]struct{}{}
		dupes            int
		failedCategories []string
		workers          sync.WaitGroup
		jobs             = make(chan string)
		workerID         int
	)

	addProducts := func(category string, products []hermes.ListedProduct) int {
		mu.Lock()
		defer mu.Unlock()
		added := 0
		for _, product := range products {
			if _, ok := seen[product.SKU]; ok {
				dupes++
				continue
			}
			seen[product.SKU] = struct{}{}
			entries = append(entries, catalogEntry{Category: category, ListedProduct: product})
			added++
		}
		return added
	}

	nextWorkerID := func() int {
		mu.Lock()
		defer mu.Unlock()
		workerID++
		return workerID
	}

	recordFailure := func(category string) {
		mu.Lock()
		defer mu.Unlock()
		failedCategories = append(failedCategories, category)
	}

	stopProgress := make(chan struct{})
	go func() {
		ticker := time.NewTicker(progressInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				mu.Lock()
				log.Printf("[%s] progress: %d unique SKUs (%d duplicates), elapsed %s", locale, len(entries), dupes, time.Since(startedAt).Round(time.Second))
				mu.Unlock()
			case <-stopProgress:
				return
			}
		}
	}()

	crawl := func(category string) {
		id := nextWorkerID()
		session := hermes.NewSession(id)
		defer session.Close()

		for offset := 0; offset <= maxOffset; offset += pageSize {
			if ctx.Err() != nil {
				return
			}

			query := hermes.ProductsQuery{Category: category, Sort: sortOrder, Offset: offset, PageSize: pageSize, Locale: locale}

			page, err := fetchPageWithRetry(ctx, id, session, query)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				log.Printf("[%s] worker=%02d category=%s offset=%d giving up after %d attempts: %v", locale, id, category, offset, maxCategoryPageAttempts, err)
				recordFailure(category)
				return
			}

			if page.Count == 0 {
				return
			}

			added := addProducts(category, page.Products)
			log.Printf("[%s] worker=%02d category=%s offset=%d got %d (%d new)", locale, id, category, offset, page.Count, added)

			if page.Total > 0 && offset+page.Count >= page.Total {
				return
			}
		}
	}

	workers.Add(threads)
	for range threads {
		go func() {
			defer workers.Done()
			for category := range jobs {
				crawl(category)
			}
		}()
	}

	for _, category := range categories {
		select {
		case jobs <- category:
		case <-ctx.Done():
		}
	}
	close(jobs)
	workers.Wait()
	close(stopProgress)

	if len(failedCategories) > 0 {
		log.Printf("[%s] WARNING: %d categories failed after retries (re-run, ideally at lower -threads): %s",
			locale, len(failedCategories), strings.Join(failedCategories, ", "))
	}

	result := catalog{
		Locale:     locale,
		Sort:       sortOrder,
		PageSize:   pageSize,
		Categories: categories,
		Count:      len(entries),
		Products:   entries,
	}
	if err := writeJSON(outPath, result); err != nil {
		return fmt.Errorf("write output: %w", err)
	}

	log.Printf("[%s] done: %d unique SKUs (%d duplicates) written to %s in %s",
		locale, len(entries), dupes, outPath, time.Since(startedAt).Round(time.Second))
	return nil
}

// fetchPageWithRetry fetches one listing page, retrying on transient errors
// (a fresh session bootstraps a new proxy IP, recovering from proxy hard-blocks).
func fetchPageWithRetry(ctx context.Context, id int, session *hermes.Session, query hermes.ProductsQuery) (hermes.ProductsPage, error) {
	var lastErr error

	for attempt := 1; attempt <= maxCategoryPageAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return hermes.ProductsPage{}, err
		}

		page, err := session.FetchProductsPage(ctx, query)
		if err == nil {
			return page, nil
		}
		lastErr = err

		if attempt == maxCategoryPageAttempts {
			break
		}

		backoff := time.Duration(attempt) * time.Second
		log.Printf("[%s] worker=%02d category=%s offset=%d attempt=%d/%d failed retry_in=%s: %v",
			query.Locale, id, query.Category, query.Offset, attempt, maxCategoryPageAttempts, backoff, err)

		timer := time.NewTimer(backoff)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return hermes.ProductsPage{}, ctx.Err()
		}
	}

	return hermes.ProductsPage{}, lastErr
}

// resolveCategories returns the category list to crawl: an explicit comma-list,
// or every category discovered from /menu when the flag is "auto" or "top".
func resolveCategories(ctx context.Context, categoriesFlag, locale string) ([]string, error) {
	mode := strings.ToLower(strings.TrimSpace(categoriesFlag))
	topOnly := mode == "top"

	if mode != "top" && mode != "auto" {
		categories := splitList(categoriesFlag)
		if len(categories) == 0 {
			return nil, fmt.Errorf("no categories provided")
		}
		return categories, nil
	}

	session := hermes.NewSession(0)
	defer session.Close()

	categories, err := session.FetchCategories(ctx, locale, topOnly)
	if err != nil {
		return nil, err
	}
	if len(categories) == 0 {
		return nil, fmt.Errorf("menu returned no categories")
	}
	return categories, nil
}

func splitList(raw string) []string {
	var out []string
	for part := range strings.SplitSeq(raw, ",") {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

func writeJSON(path string, value any) error {
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	defer file.Close()

	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}
