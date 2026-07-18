package tool

import (
	"bytes"
	"encoding/json"
)

func newJSONReader(value json.RawMessage) *bytes.Reader {
	return bytes.NewReader(value)
}
