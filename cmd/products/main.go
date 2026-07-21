// Command products reads a SKU catalog (produced by cmd/skus) and fetches full
// product detail for each SKU from the bck.hermes.com product API, writing the
// results to a JSON file.
//
// Pass -locale all to scrape every market: it reads skus-<locale>.json and writes
// products-<locale>.json for each of hermes.Locales, then prints one aggregate
// summary with the blog metrics.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"hermes"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	maxScrapeAttempts = 3
	progressInterval  = 30 * time.Second
)

type scrapeResult struct {
	workerID int
	sku      string
	result   []byte
	err      error
	attempts int
}

type failedProduct struct {
	SKU      string `json:"sku"`
	Attempts int    `json:"attempts"`
	Error    string `json:"error"`
}

func main() {
	inPath := flag.String("in", "skus.json", "Input SKU catalog (from cmd/skus) or a JSON array of SKUs; with -locale all, reads skus-<locale>.json")
	skusFlag := flag.String("skus", "", "Comma-separated SKUs to fetch instead of reading -in")
	localeFlag := flag.String("locale", "", `Locale override; "all" scrapes every market (skus-<locale>.json -> products-<locale>.json)`)
	threads := flag.Int("threads", 8, "Number of concurrent scrape sessions")
	outPath := flag.String("out", "products.jsonl", "JSONL output path (one raw /product response per line)")
	failPath := flag.String("failures", "product-failures.json", "JSON path for permanently failed SKUs")
	flag.Parse()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	interrupts := make(chan os.Signal, 1)
	signal.Notify(interrupts, os.Interrupt)
	defer signal.Stop(interrupts)
	go func() {
		<-interrupts
		log.Printf("received interrupt; canceling scrape")
		cancel()
	}()

	startedAll := time.Now()
	var totalSKUs int
	var totalSucceeded, totalFailed uint64

	if strings.EqualFold(strings.TrimSpace(*localeFlag), "all") {
		for _, loc := range hermes.Locales {
			if ctx.Err() != nil {
				break
			}
			in := withLocaleSuffix(*inPath, loc)
			skus, _, err := loadSKUs(in, "")
			if err != nil {
				log.Printf("[%s] load %s: %v", loc, in, err)
				continue
			}
			if len(skus) == 0 {
				log.Printf("[%s] no SKUs in %s; skipping", loc, in)
				continue
			}
			ok, bad, err := scrapeSKUs(ctx, skus, loc, *threads, withLocaleSuffix(*outPath, loc), withLocaleSuffix(*failPath, loc))
			if err != nil {
				log.Printf("[%s] %v", loc, err)
			}
			totalSKUs += len(skus)
			totalSucceeded += ok
			totalFailed += bad
		}
	} else {
		skus, catalogLocale, err := loadSKUs(*inPath, *skusFlag)
		if err != nil {
			log.Fatalf("load SKUs: %v", err)
		}
		if len(skus) == 0 {
			log.Fatalf("no SKUs to scrape")
		}
		locale := *localeFlag
		if locale == "" {
			locale = catalogLocale
		}
		if locale == "" {
			locale = "us_en"
		}
		ok, bad, err := scrapeSKUs(ctx, skus, locale, *threads, *outPath, *failPath)
		if err != nil {
			log.Fatalf("scrape SKUs: %v", err)
		}
		totalSKUs = len(skus)
		totalSucceeded, totalFailed = ok, bad
	}

	printAggregateSummary(startedAll, totalSKUs, totalSucceeded, totalFailed)
}

func withLocaleSuffix(path, locale string) string {
	ext := filepath.Ext(path)
	return strings.TrimSuffix(path, ext) + "-" + locale + ext
}

func scrapeSKUs(ctx context.Context, skus []string, locale string, threads int, outPath, failPath string) (uint64, uint64, error) {
	if threads > len(skus) {
		threads = len(skus)
	}
	if threads < 1 {
		threads = 1
	}

	file, err := os.Create(outPath)
	if err != nil {
		return 0, 0, fmt.Errorf("create output file: %w", err)
	}
	defer file.Close()

	jobs := make(chan string)
	results := make(chan scrapeResult)

	var completed uint64
	var succeeded uint64
	var failed uint64
	startedAt := time.Now()

	log.Printf("[%s] scraping %d SKUs with %d threads", locale, len(skus), threads)

	stopProgress := make(chan struct{})
	go logProgressEvery(stopProgress, startedAt, len(skus), locale, &completed, &succeeded, &failed)

	var workers sync.WaitGroup
	workers.Add(threads)
	for workerID := 1; workerID <= threads; workerID++ {
		go func(workerID int) {
			defer workers.Done()
			session := hermes.NewSession(workerID)
			defer session.Close()

			for sku := range jobs {
				result, attempts, err := fetchWithRetry(ctx, workerID, session, sku, locale)
				results <- scrapeResult{workerID: workerID, sku: sku, result: result, err: err, attempts: attempts}
			}
		}(workerID)
	}

	go func() {
		defer close(jobs)
		for _, sku := range skus {
			select {
			case jobs <- sku:
			case <-ctx.Done():
				return
			}
		}
	}()

	go func() {
		workers.Wait()
		close(results)
	}()

	failures, writeErr := writeResults(file, results, &completed, &succeeded, &failed)
	close(stopProgress)

	log.Printf("[%s] done: %d/%d succeeded, %d failed, elapsed %s",
		locale, atomic.LoadUint64(&succeeded), len(skus), atomic.LoadUint64(&failed), time.Since(startedAt).Round(time.Second))

	if len(failures) > 0 {
		if err := writeJSONFile(failPath, failures); err != nil {
			log.Printf("[%s] write failures: %v", locale, err)
		} else {
			log.Printf("[%s] wrote %d failures to %s", locale, len(failures), failPath)
		}
	}

	return atomic.LoadUint64(&succeeded), atomic.LoadUint64(&failed), writeErr
}

// writeResults streams each successful product as one line of JSONL: the raw
// /product response, compacted to a single line. Failures are collected and
// returned. Because each record is an independent line, an interrupted run still
// leaves a valid file.
func writeResults(writer io.Writer, results <-chan scrapeResult, completed, succeeded, failed *uint64) ([]failedProduct, error) {
	failures := make([]failedProduct, 0)

	for result := range results {
		atomic.AddUint64(completed, 1)

		fail := func(err error) {
			atomic.AddUint64(failed, 1)
			failures = append(failures, failedProduct{SKU: result.sku, Attempts: result.attempts, Error: err.Error()})
			log.Printf("worker=%02d sku=%s failed after %d attempts: %v", result.workerID, result.sku, result.attempts, err)
		}

		if result.err != nil {
			fail(result.err)
			continue
		}

		var line bytes.Buffer
		if err := json.Compact(&line, result.result); err != nil {
			fail(fmt.Errorf("invalid JSON response: %w", err))
			continue
		}
		line.WriteByte('\n')

		if _, err := writer.Write(line.Bytes()); err != nil {
			return failures, err
		}
		atomic.AddUint64(succeeded, 1)
	}

	return failures, nil
}

func fetchWithRetry(ctx context.Context, workerID int, session *hermes.Session, sku, locale string) ([]byte, int, error) {
	var lastErr error

	for attempt := 1; attempt <= maxScrapeAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, attempt - 1, err
		}

		body, err := session.FetchProduct(ctx, sku, locale)
		if err == nil {
			return body, attempt, nil
		}
		lastErr = err

		if attempt == maxScrapeAttempts {
			break
		}

		backoff := time.Duration(attempt) * time.Second
		log.Printf("worker=%02d sku=%s attempt=%d/%d failed retry_in=%s: %v", workerID, sku, attempt, maxScrapeAttempts, backoff, err)

		timer := time.NewTimer(backoff)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return nil, attempt, ctx.Err()
		}
	}

	return nil, maxScrapeAttempts, lastErr
}

func logProgressEvery(done <-chan struct{}, startedAt time.Time, total int, locale string, completed, succeeded, failed *uint64) {
	ticker := time.NewTicker(progressInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			logProgress(startedAt, total, locale, completed, succeeded, failed)
		case <-done:
			return
		}
	}
}

func logProgress(startedAt time.Time, total int, locale string, completed, succeeded, failed *uint64) {
	done := atomic.LoadUint64(completed)
	ok := atomic.LoadUint64(succeeded)
	bad := atomic.LoadUint64(failed)
	elapsed := time.Since(startedAt)

	rate := 0.0
	if elapsed > 0 {
		rate = float64(done) / elapsed.Minutes()
	}
	successRate := 0.0
	if done > 0 {
		successRate = float64(ok) / float64(done) * 100
	}

	log.Printf(
		"[%s] progress: %d/%d complete, success rate %.1f%%, failed %d, rate %.1f SKUs/min, elapsed %s",
		locale, done, total, successRate, bad, rate, elapsed.Round(time.Second),
	)
}

func printAggregateSummary(startedAt time.Time, total int, succeeded, failed uint64) {
	telemetry := hermes.GetTelemetrySnapshot()
	sensors := telemetry.DataDomeInterstitialCalls + telemetry.DataDomeSliderCalls + telemetry.DataDomeTagsCalls

	log.Printf(
		"summary: %d products, succeeded %d, failed %d, elapsed %s, bandwidth %s, datadome calls interstitial=%d slider=%d tags=%d (sensors=%d)",
		total, succeeded, failed, time.Since(startedAt).Round(time.Second), hermes.FormatBandwidth(telemetry.BandwidthBytes),
		telemetry.DataDomeInterstitialCalls, telemetry.DataDomeSliderCalls, telemetry.DataDomeTagsCalls, sensors,
	)

	if succeeded == 0 {
		return
	}

	// Blog metrics. Bandwidth is billed per decimal GB (e.g. by proxy vendors),
	// so pages-per-GB uses 1e9 bytes.
	bytes := float64(telemetry.BandwidthBytes)
	pages := float64(succeeded)
	line := fmt.Sprintf("blog metrics: %.1f KB/page, %.0f pages/GB", bytes/1024/pages, pages/(bytes/1e9))
	if sensors > 0 {
		line += fmt.Sprintf(", %.0f pages/1000 sensors (%.3f sensors/page)", pages/float64(sensors)*1000, float64(sensors)/pages)
	}
	log.Print(line)
}

// loadSKUs reads SKUs from an explicit comma-separated flag, or from a JSON file
// that is either a cmd/skus catalog, an array of product objects, or an array of
// SKU strings.
func loadSKUs(path, skusFlag string) ([]string, string, error) {
	if skusFlag != "" {
		return dedupe(splitList(skusFlag)), "", nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, "", err
	}

	// Catalog form: {"locale": "...", "products": [{"sku": "..."}]}
	var catalog struct {
		Locale   string `json:"locale"`
		Products []struct {
			SKU string `json:"sku"`
		} `json:"products"`
	}
	if err := json.Unmarshal(data, &catalog); err == nil && len(catalog.Products) > 0 {
		skus := make([]string, 0, len(catalog.Products))
		for _, product := range catalog.Products {
			if product.SKU != "" {
				skus = append(skus, product.SKU)
			}
		}
		return dedupe(skus), catalog.Locale, nil
	}

	// Array of objects with a sku field.
	var objects []struct {
		SKU string `json:"sku"`
	}
	if err := json.Unmarshal(data, &objects); err == nil && len(objects) > 0 {
		skus := make([]string, 0, len(objects))
		for _, object := range objects {
			if object.SKU != "" {
				skus = append(skus, object.SKU)
			}
		}
		if len(skus) > 0 {
			return dedupe(skus), "", nil
		}
	}

	// Plain array of strings.
	var strdata []string
	if err := json.Unmarshal(data, &strdata); err == nil {
		return dedupe(strdata), "", nil
	}

	return nil, "", fmt.Errorf("could not parse SKUs from %s", path)
}

func dedupe(values []string) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func splitList(raw string) []string {
	return strings.Split(raw, ",")
}

func writeJSONFile(path string, value any) error {
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	defer file.Close()

	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}
