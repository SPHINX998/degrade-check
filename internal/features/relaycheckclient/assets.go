package relaycheckclient

import (
	_ "embed"
	"encoding/json"
)

//go:embed ui/index.html
var pageHTML []byte

//go:embed ui/promo.json
var promoJSON []byte

// builtInPromo is the promotion when check-api cannot be reached.
func builtInPromo() Promo {
	var promo Promo
	_ = json.Unmarshal(promoJSON, &promo)
	return promo
}
