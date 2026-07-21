package hermes

import (
	"fmt"
	"net/url"
	"strings"
)

// ProductsQuery describes one page of a category listing on the products API,
// e.g. https://bck.hermes.com/products?category=MEN&sort=relevance&offset=48&pagesize=48&locale=us_en
type ProductsQuery struct {
	Category string
	Sort     string
	Offset   int
	PageSize int
	Locale   string
}

// URL renders the query preserving the parameter order the browser sends. The
// offset parameter is omitted when zero, matching the browser's first-page request.
func (q ProductsQuery) URL() string {
	sort := q.Sort
	if sort == "" {
		sort = "relevance"
	}

	offset := ""
	if q.Offset > 0 {
		offset = fmt.Sprintf("offset=%d&", q.Offset)
	}

	return fmt.Sprintf(
		"%s?category=%s&sort=%s&%spagesize=%d&locale=%s",
		APIProductsEndpoint,
		queryEscape(q.Category),
		queryEscape(sort),
		offset,
		q.PageSize,
		queryEscape(q.Locale),
	)
}

// ProductURL renders a single-product API URL, e.g.
// https://bck.hermes.com/product?productsku=H100044FPG2&locale=us_en
// SKUs can contain spaces (e.g. "H252048N 6457"), which are encoded as %20.
func ProductURL(sku, locale string) string {
	return fmt.Sprintf(
		"%s?productsku=%s&locale=%s",
		APIProductEndpoint,
		queryEscape(sku),
		queryEscape(locale),
	)
}

// SyncFormURL renders the /sync-form URL that mints the x-xsrf-token cookie.
func SyncFormURL(locale string) string {
	return fmt.Sprintf("%s?locale=%s", APISyncFormEndpoint, queryEscape(locale))
}

// MenuURL renders the /menu URL that returns the full category tree.
func MenuURL(locale string) string {
	return fmt.Sprintf("%s?locale=%s", APIMenuEndpoint, queryEscape(locale))
}

// ParseMenuCategories walks the /menu response and returns product category
// codes (pimCode), deduped and in menu order. Content nodes (type "story") are
// skipped since they are editorial pages, not listings. When topOnly is true,
// only the top-level categories are returned — those aggregate their whole
// sub-tree, so the subcategories add essentially no new SKUs.
func ParseMenuCategories(body []byte, topOnly bool) []string {
	root := piDecodeObject(body)
	if len(root) == 0 {
		return nil
	}

	var codes []string
	seen := map[string]struct{}{}

	add := func(entry map[string]any) {
		code := piAsString(entry["pimCode"])
		if code == "" || piAsString(entry["type"]) == "story" {
			return
		}
		if _, ok := seen[code]; !ok {
			seen[code] = struct{}{}
			codes = append(codes, code)
		}
	}

	var walk func(nodes []any)
	walk = func(nodes []any) {
		for _, node := range nodes {
			entry := piAsMap(node)
			if len(entry) == 0 {
				continue
			}
			add(entry)
			if !topOnly {
				walk(piAsArray(entry["subcategories"]))
			}
		}
	}

	walk(piAsArray(root["mainMenu"]))
	return codes
}

// queryEscape escapes a query value like the browser does, encoding spaces as
// %20 rather than + (Hermès SKUs contain spaces).
func queryEscape(value string) string {
	return strings.ReplaceAll(url.QueryEscape(value), "+", "%20")
}

// ProductsPage is one decoded page of a category listing.
type ProductsPage struct {
	Category string          `json:"category,omitempty"`
	Total    int             `json:"total,omitempty"`
	Offset   int             `json:"offset,omitempty"`
	PageSize int             `json:"pagesize,omitempty"`
	Count    int             `json:"count"`
	Products []ListedProduct `json:"products"`
}

// ListedProduct is the relevant subset of a listing entry. The SKU is what the
// product scraper consumes; the rest is captured for the exported catalog.
type ListedProduct struct {
	SKU            string   `json:"sku"`
	Title          string   `json:"title,omitempty"`
	DisplayName    string   `json:"display_name,omitempty"`
	Slug           string   `json:"slug,omitempty"`
	URL            string   `json:"url,omitempty"`
	Image          string   `json:"image,omitempty"`
	Price          *float64 `json:"price,omitempty"`
	ProductCode    string   `json:"product_code,omitempty"`
	DepartmentCode string   `json:"department_code,omitempty"`
	Categories     []string `json:"categories,omitempty"`
}

// ParseProductsListing decodes a /products response. The exact schema is parsed
// defensively (the product array and pagination fields are looked up under
// several common key names) so it tolerates schema differences across locales.
func ParseProductsListing(body []byte, locale string) (ProductsPage, error) {
	var root map[string]any
	if err := piDecodeJSON(body, &root); err != nil {
		return ProductsPage{}, fmt.Errorf("decode products listing: %w", err)
	}

	baseHref := localeBaseHref(locale)
	page := ProductsPage{
		Total:    firstInt(root, "total", "totalCount", "totalResults", "nbHits", "count"),
		Offset:   firstInt(root, "offset"),
		PageSize: firstInt(root, "pagesize", "pageSize"),
	}

	for _, item := range findItemsArray(root) {
		product := piAsMap(item)
		if len(product) == 0 {
			continue
		}

		sku := piFirstNonEmpty(
			piAsString(product["sku"]),
			piAsString(product["productsku"]),
			piAsString(product["skuWithoutSize"]),
		)
		if sku == "" {
			continue
		}

		image := piAsString(product["image"])
		if image == "" {
			image = piFirstAssetURL(product["assets"])
		}

		page.Products = append(page.Products, ListedProduct{
			SKU:            sku,
			Title:          piFirstNonEmpty(piAsString(product["title"]), piAsString(product["display_name_list"]), piAsString(product["displayNameList"])),
			DisplayName:    piFirstNonEmpty(piAsString(product["display_name_list"]), piAsString(product["displayNameList"])),
			Slug:           piAsString(product["slug"]),
			URL:            piNormalizeHermesURL(piFirstNonEmpty(piAsString(product["url"]), piAsString(product["path"])), baseHref),
			Image:          piNormalizeAssetURL(image),
			Price:          piFloatPtr(product["price"]),
			ProductCode:    piAsString(product["productCode"]),
			DepartmentCode: piAsString(product["departmentCode"]),
			Categories:     piAsStringSlice(product["categories"]),
		})
	}

	page.Count = len(page.Products)
	return page, nil
}

// findItemsArray locates the product array in a /products response. The
// confirmed schema nests it at products.items, but several fallbacks are tried
// so the parser tolerates schema differences.
func findItemsArray(root map[string]any) []any {
	if nested := piAsMap(root["products"]); nested != nil {
		if arr := firstArray(nested, "items", "results", "hits", "products"); arr != nil {
			return arr
		}
	}
	if arr := piAsArray(root["products"]); arr != nil {
		return arr
	}
	if nested := piAsMap(root["data"]); nested != nil {
		if arr := firstArray(nested, "items", "products", "results", "hits"); arr != nil {
			return arr
		}
	}
	return firstArray(root, "items", "results", "hits", "data")
}

func firstInt(m map[string]any, keys ...string) int {
	for _, key := range keys {
		if value, ok := m[key]; ok {
			if n := piFloatPtr(value); n != nil {
				return int(*n)
			}
		}
	}
	return 0
}

func firstArray(m map[string]any, keys ...string) []any {
	for _, key := range keys {
		if arr := piAsArray(m[key]); arr != nil {
			return arr
		}
	}
	return nil
}

func localeBaseHref(locale string) string {
	country, lang, ok := splitLocale(locale)
	if !ok {
		return ""
	}
	return "/" + country + "/" + lang
}

func splitLocale(locale string) (country string, lang string, ok bool) {
	parts := strings.SplitN(locale, "_", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}
