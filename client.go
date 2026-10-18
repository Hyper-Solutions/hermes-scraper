package hermes

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"math"
	"math/rand"
	"os"
	"strings"
	"sync/atomic"

	"github.com/Hyper-Solutions/hyper-sdk-go/v2"
	http "github.com/bogdanfinn/fhttp"
	"github.com/bogdanfinn/fhttp/cookiejar"
	tls_client "github.com/bogdanfinn/tls-client"
	"github.com/bogdanfinn/tls-client/profiles"
	"github.com/jackc/puddle/v2"
)

type wrappedClient struct {
	Client                    tls_client.HttpClient
	Ip                        string
	AcceptLanguage            string
	isFirstRequest            bool
	bootstrapped              bool
	headers                   *hyper.Headers
	apiCalls                  int
	datadomeInterstitialCalls atomic.Uint64
	datadomeSliderCalls       atomic.Uint64
	datadomeTagsCalls         atomic.Uint64
}

func init() {
	constructor := func(ctx context.Context) (*wrappedClient, error) {
		var err error
		defer func() {
			if r := recover(); r != nil {
				err = errors.New("unknown panic")
			}
		}()

		proxy := os.Getenv("HTTP_PROXY")

		if strings.Contains(proxy, "SESSION") {
			const chars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ1234567890"
			var sessionId strings.Builder
			for range 7 {
				sessionId.WriteByte(chars[rand.Intn(len(chars))])
			}
			proxy = strings.Replace(proxy, "SESSION", sessionId.String(), 1)
		}

		jar, _ := cookiejar.New(nil)

		client, err := tls_client.NewHttpClient(tls_client.NewNoopLogger(),
			tls_client.WithTimeoutSeconds(35),
			tls_client.WithClientProfile(profiles.Chrome_152),
			tls_client.WithDisableHttp3(),
			tls_client.WithNotFollowRedirects(),
			tls_client.WithCookieJar(jar),
			tls_client.WithRandomTLSExtensionOrder(),
			tls_client.WithProxyUrl(proxy),
			tls_client.WithBandwidthTracker(),
		)
		if err != nil {
			return nil, err
		}

		ip, err := GetIp(ctx, client)
		if err != nil {
			return nil, err
		}

		c := &wrappedClient{
			Client:         client,
			Ip:             ip,
			AcceptLanguage: "en-US,en;q=0.9",
			isFirstRequest: true,
		}
		registerClient(c)

		return c, nil
	}

	destructor := func(c *wrappedClient) {
		retireClient(c)
		if c != nil && c.Client != nil {
			c.Client.CloseIdleConnections()
		}
	}

	var err error
	pool, err = puddle.NewPool(&puddle.Config[*wrappedClient]{Constructor: constructor, Destructor: destructor, MaxSize: math.MaxInt32})
	if err != nil {
		log.Fatal(err)
	}
}

var pool *puddle.Pool[*wrappedClient]

func GetIp(ctx context.Context, client tls_client.HttpClient) (string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", "https://ip.hypersolutions.co/ip", nil)
	if err != nil {
		return "", err
	}
	req.Header = http.Header{
		"x-api-key":         {os.Getenv("API_KEY")},
		"content-type":      {"application/json"},
		http.HeaderOrderKey: {"x-api-key", "content-type"},
	}

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var response struct {
		IP string `json:"ip"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
		return "", err
	}

	return response.IP, nil
}
