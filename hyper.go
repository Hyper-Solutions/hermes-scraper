package hermes

import (
	"os"

	"github.com/Hyper-Solutions/hyper-sdk-go/v2"
)

var HyperSdk = hyper.NewSession(os.Getenv("API_KEY")) // you'll need an api key for this, https://hypersolutions.co/keys
