package host

import (
	"encoding/json"
	"fmt"
)

type plainPart Part

func (p Part) MarshalJSON() ([]byte, error) {
	if p.Type == "text" {
		return json.Marshal(struct {
			plainPart
			Text string `json:"text"`
		}{plainPart: plainPart(p), Text: p.Text})
	}
	return json.Marshal(plainPart(p))
}

func (p *Part) UnmarshalJSON(raw []byte) error {
	var part plainPart
	if err := decode(raw, &part); err != nil {
		return err
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		return err
	}
	var names []string
	switch part.Type {
	case "text":
		names = []string{"type", "text"}
	case "tool_call":
		names = []string{"type", "tool_call_id", "name", "arguments"}
	case "tool_result":
		names = []string{"type", "tool_call_id", "result"}
	default:
		return fault("UNSUPPORTED_CAPABILITY")
	}
	if len(object) != len(names) {
		return fmt.Errorf("invalid part fields")
	}
	for _, name := range names {
		if _, ok := object[name]; !ok {
			return fmt.Errorf("missing part field")
		}
	}
	*p = Part(part)
	return nil
}
