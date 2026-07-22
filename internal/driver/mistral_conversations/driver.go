package mistral_conversations

import (
	"net/http"

	"Eylu/internal/driver/webnative"
)

const Name = "mistral_conversations"

func New(client *http.Client) *webnative.Driver {
	return webnative.NewNamed(client, webnative.DialectMistral, Name)
}
