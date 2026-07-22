package perplexity_agent

import (
	"net/http"

	"Eylu/internal/driver/webnative"
)

const Name = "perplexity_agent"

func New(client *http.Client) *webnative.Driver {
	return webnative.NewNamed(client, webnative.DialectPerplexity, Name)
}
