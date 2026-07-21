# hermes-scraper

A request-based scraper for the Hermès catalog ([hermes.com](https://www.hermes.com)), which sits behind [DataDome](https://datadome.co/). No browser, no headless Chrome, no page rendering: it warms a session through DataDome once, then reads the `bck.hermes.com` JSON API directly. Each product weighs about 37 KB on the wire.

It is written in Go and uses [`bogdanfinn/tls-client`](https://github.com/bogdanfinn/tls-client) for a browser-grade TLS fingerprint, `fhttp` for exact Chrome header ordering, and the [Hyper Solutions API](https://hypersolutions.co/) for the DataDome sensor data.

> This project is for educational purposes and uses publicly available product data.

## How it works

Everything runs over plain HTTP requests. A fresh session is bootstrapped once and then reused for many API calls:

1. **GET the homepage.** If DataDome challenges it (a `403` with a `dd` object, or an XHR challenge carrying a `geo.captcha-delivery.com` device link), solve it (interstitial or slider) through the Hyper Solutions API.
2. **Post the DataDome tags** (`ch` then `le`), like a real page load.
3. **GET `/sync-form`** to mint the `x-xsrf-token` cookie that subsequent API calls echo back as a header.
4. **Call the JSON API.** `bck.hermes.com/menu` (category tree) and `bck.hermes.com/products` (paginated listings) to collect SKUs, then `bck.hermes.com/product` for full detail. Sessions rotate after a set number of calls.

The only calls that hit the Hyper Solutions API are the DataDome solves and tags posts; everything else is direct HTTP.

## Requirements

- Go 1.26+
- A [Hyper Solutions API key](https://hypersolutions.co/keys) (for DataDome)
- A sticky/session residential proxy (the IP you scrape from must be stable per session)

## Setup

```sh
export API_KEY="your-hyper-solutions-key"
export HTTP_PROXY="http://user-SESSION:pass@host:port"   # "SESSION" is replaced with a random id per session
```

The proxy must be sticky. If the proxy string contains the literal token `SESSION`, the scraper substitutes a fresh random id per session so each session holds its own IP.

## Usage

Two stages: collect SKUs, then fetch full product detail.

### One market

```sh
go run ./cmd/skus     -locale us_en -out skus.json
go run ./cmd/products -in skus.json -out products.jsonl
```

### Every market

`cmd/skus` defaults to `-locale all`, which crawls every market in `hermes.Locales` (39 storefronts), writing `skus-<locale>.json` per market. `cmd/products -locale all` reads those and writes `products-<locale>.json`, printing one aggregate summary at the end.

```sh
go run ./cmd/skus     -locale all
go run ./cmd/products -locale all
```

### Quick check

```sh
go run ./cmd/products -skus H100044FPG2 -locale us_en
```

## Flags

**`cmd/skus`** — discover categories and collect SKUs:

| Flag | Default | Meaning |
|---|---|---|
| `-locale` | `all` | Locale (e.g. `us_en`), or `all` for every market |
| `-categories` | `auto` | `auto` (every category), `top` (top-level only, which aggregates its sub-tree), or a comma-separated list |
| `-pagesize` | `144` | Listing page size |
| `-threads` | `8` | Concurrent category crawlers |
| `-out` | `skus.json` | Output path (a `-<locale>` suffix is added per market when `-locale all`) |

**`cmd/products`** — fetch full product detail per SKU:

| Flag | Default | Meaning |
|---|---|---|
| `-in` | `skus.json` | SKU catalog from `cmd/skus`, or a JSON array of SKUs |
| `-skus` | | Comma-separated SKUs, instead of `-in` |
| `-locale` | | Locale override, or `all` for every market (reads `skus-<locale>.json`) |
| `-threads` | `8` | Concurrent scrape sessions |
| `-out` | `products.jsonl` | JSONL output path (one raw `/product` response per line) |

Higher `-threads` means more concurrent sessions, and each session bootstraps through DataDome (a solve plus tag posts), so more threads means more session bootstraps and more sensor calls. 8 is a good balance of speed and cost.

## Output

`cmd/skus` writes a catalog of SKUs plus basic listing fields. `cmd/products` writes **JSONL**: one raw `/product` response per line, unnormalized (exactly what the API returned, with the complete object: price, every color/finish/size variant, stock, breadcrumbs, dimensions, material, and more). Because each record is its own line, an interrupted run still leaves a valid file, and the output streams with `jq`, `wc -l`, and friends. At the end of a run it prints the bandwidth, DataDome sensor-call counts, and derived per-product ratios.

## Notes on completeness

- Product URLs come from the `bck.hermes.com/menu` category tree, not the sitemap (which is not a reliable list of purchasable products). A top-level category already aggregates its whole sub-tree, so `-categories top` is nearly complete and much cheaper; `-categories auto` walks every level and dedupes.
- Same-country language variants (e.g. `ca_en` and `ca_fr`) are the same catalog in a different display language.

## License

See [LICENSE](LICENSE).
