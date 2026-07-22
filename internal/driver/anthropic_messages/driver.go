package anthropic_messages

import (
	"net/http"

	"Eylu/internal/driver/webnative"
)

const Name = "anthropic_messages"

func New(client *http.Client) *webnative.Driver {
	return webnative.NewNamed(client, webnative.DialectAnthropic, Name)
}
