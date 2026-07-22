package gemini_interactions

import (
	"net/http"

	"Eylu/internal/driver/webnative"
)

const Name = "gemini_interactions"

func New(client *http.Client) *webnative.Driver {
	return webnative.NewNamed(client, webnative.DialectGemini, Name)
}
