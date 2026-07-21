package hermes

import (
	"context"

	"github.com/jackc/puddle/v2"
)

// Session is a warmed browsing context: it lazily loads and solves the homepage,
// posts the DataDome tags, and then reuses the resulting cookies to call the
// bck.hermes.com JSON API until the underlying client is retired.
type Session struct {
	workerID int
	resource *puddle.Resource[*wrappedClient]
}

func NewSession(workerID int) *Session {
	return &Session{workerID: workerID}
}

// FetchProductsPage retrieves one page of a category listing.
func (s *Session) FetchProductsPage(ctx context.Context, query ProductsQuery) (ProductsPage, error) {
	body, err := s.apiCall(ctx, query.URL(), query.Locale)
	if err != nil {
		return ProductsPage{}, err
	}

	page, err := ParseProductsListing(body, query.Locale)
	if err != nil {
		return ProductsPage{}, err
	}
	page.Category = query.Category
	if page.Offset == 0 {
		page.Offset = query.Offset
	}
	if page.PageSize == 0 {
		page.PageSize = query.PageSize
	}
	return page, nil
}

// FetchCategories fetches the /menu tree and returns product category codes.
// When topOnly is true, only top-level categories are returned (they aggregate
// their sub-tree); otherwise every level is returned.
func (s *Session) FetchCategories(ctx context.Context, locale string, topOnly bool) ([]string, error) {
	body, err := s.apiCall(ctx, MenuURL(locale), locale)
	if err != nil {
		return nil, err
	}
	return ParseMenuCategories(body, topOnly), nil
}

// FetchProduct retrieves full product detail for a single SKU and returns the
// raw /product JSON response, unnormalized.
func (s *Session) FetchProduct(ctx context.Context, sku string, locale string) ([]byte, error) {
	return s.apiCall(ctx, ProductURL(sku, locale), locale)
}

func (s *Session) apiCall(ctx context.Context, apiURL string, locale string) ([]byte, error) {
	client, err := s.client(ctx)
	if err != nil {
		return nil, err
	}

	if err := bootstrapSession(ctx, client, s.workerID, locale); err != nil {
		s.destroyCurrentClient("destroyed session after bootstrap error")
		return nil, err
	}

	body, err := apiGet(ctx, client, apiURL, s.workerID)
	if err != nil {
		s.destroyCurrentClient("destroyed session after api error")
		return nil, err
	}

	return body, nil
}

func (s *Session) Close() {
	if s.resource == nil {
		return
	}

	s.resource.Release()
	s.resource = nil
}

func (s *Session) client(ctx context.Context) (*wrappedClient, error) {
	if s.resource == nil {
		resource, err := pool.Acquire(ctx)
		if err != nil {
			return nil, err
		}
		s.resource = resource
	}

	return s.resource.Value(), nil
}

func (s *Session) destroyCurrentClient(reason string) {
	if s.resource == nil {
		return
	}

	apiCalls := s.resource.Value().apiCalls
	s.resource.Destroy()
	s.resource = nil
	scrapeLogf(s.workerID, "%s api_calls=%d", reason, apiCalls)
}
