package hermes

const (
	UserAgent   = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/155.0.0.0 Safari/537.36"
	ClientHints = `"Google Chrome";v="155", "Chromium";v="155", "Not(A:Brand";v="24"`

	// HomepageURL is warmed (challenge solved, tags posted) before hitting the
	// JSON API. SiteOrigin/SiteReferer are the origin the bck.hermes.com XHR
	// calls are made from.
	HomepageURL = "https://www.hermes.com/us/en/"
	SiteOrigin  = "https://www.hermes.com"
	SiteReferer = "https://www.hermes.com/"

	// bck.hermes.com JSON API endpoints.
	APIProductsEndpoint = "https://bck.hermes.com/products"
	APIProductEndpoint  = "https://bck.hermes.com/product"
	APISyncFormEndpoint = "https://bck.hermes.com/sync-form"
	APIMenuEndpoint     = "https://bck.hermes.com/menu"
)
