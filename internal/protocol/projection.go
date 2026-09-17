package protocol

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
)

// Bounds of the model-visible projection of a tool result.
//
// They exist because a model request can only carry text while a tool can return
// anything: without a bound, the projection of an image or of a deep JSON document
// is decided by the tool rather than by the context budget.
const (
	// MaxProjectedStructuredBytes bounds the structured content one request carries.
	MaxProjectedStructuredBytes = 8 << 10
	// MaxProjectedTextBlockBytes bounds one rich text block taken from a result.
	MaxProjectedTextBlockBytes = 8 << 10
	// MaxProjectedBlocks bounds how many content blocks one request describes.
	MaxProjectedBlocks = 32
)

// ToolResultProjection is the model-visible form of one tool result.
type ToolResultProjection struct {
	// Text is the exact text a request carries for this result.
	Text string
	// Omitted reports that the stored result held something this projection did
	// not carry in full: binary content, oversized structured content, or more
	// blocks than the bound allows. It is what lets a caller mark the request as
	// incomplete without guessing.
	Omitted bool
}

// ToolResultText returns the text a model is shown for one tool result.
func ToolResultText(result ToolResult) string {
	return ProjectToolResult(result).Text
}

// ProjectToolResult projects one stored tool result onto the text a model request
// carries.
//
// A result has two lifetimes. The host stores everything the tool returned - the
// historical text, the multimodal blocks and the structured JSON - and keeps it in
// the transcript and the session. A model request can carry one text field per
// result, and everything it carries is charged to the context budget. The
// projection is therefore defined once, here, and the ledger and the drivers both
// use it: a driver that serialized the rich fields by itself would send content
// nobody measured, and a ledger that measured only the text field would charge for
// less than was sent.
//
// The rules are:
//
//   - A text block that merely repeats the result's own text is dropped, so one
//     answer is not sent twice.
//   - Binary blocks are described, never inlined: the projection carries the type,
//     the media type, the byte count and a digest, not megabytes of base64 in a
//     text field. A provider that can accept an image gets it through a driver's
//     own multimodal path, not through this text.
//   - Structured content that does not fit the bound is replaced by a valid JSON
//     summary of what was dropped, so the projection is never invalid JSON.
//   - A result that is only text projects to that text, which is what every driver
//     already sent.
func ProjectToolResult(result ToolResult) ToolResultProjection {
	blocks := make([]projectedBlock, 0, len(result.ContentBlocks))
	omitted := false
	// A text block whose text the result's own text already carries is not sent
	// twice. The legacy text field of an MCP result is the concatenation of those
	// blocks, so a request that carried both would pay for the same answer twice
	// and the model would read it twice.
	repeated := make(map[string]struct{}, 1)
	if trimmed := strings.TrimSpace(result.Content); trimmed != "" {
		repeated[trimmed] = struct{}{}
	}
	for _, block := range result.ContentBlocks {
		if block.Type != ContentText {
			continue
		}
		if trimmed := strings.TrimSpace(block.Text); trimmed != "" && strings.Contains(result.Content, trimmed) {
			repeated[trimmed] = struct{}{}
		}
	}
	for index, block := range result.ContentBlocks {
		if index >= MaxProjectedBlocks {
			omitted = true
			break
		}
		projected, keep, blockOmitted := projectContentBlock(block, repeated)
		// A block whose payload was left out is still described: the omission is
		// about what the request carries, not about whether the model is told that
		// something was returned.
		if blockOmitted {
			omitted = true
		}
		if !keep {
			continue
		}
		blocks = append(blocks, projected)
		if projected.Text != "" {
			repeated[strings.TrimSpace(projected.Text)] = struct{}{}
		}
	}
	structured, structuredOmitted := projectStructuredContent(result.StructuredContent)
	omitted = omitted || structuredOmitted
	if len(blocks) == 0 && len(structured) == 0 {
		return ToolResultProjection{Text: result.Content, Omitted: omitted}
	}
	envelope := struct {
		Content           string           `json:"content,omitempty"`
		ContentBlocks     []projectedBlock `json:"content_blocks,omitempty"`
		StructuredContent json.RawMessage  `json:"structured_content,omitempty"`
	}{Content: result.Content, ContentBlocks: blocks, StructuredContent: structured}
	encoded, err := json.Marshal(envelope)
	if err != nil {
		// The envelope is built from JSON-encodable values, so this is unreachable
		// in practice; falling back to the text keeps the request valid text.
		return ToolResultProjection{Text: result.Content, Omitted: true}
	}
	return ToolResultProjection{Text: string(encoded), Omitted: omitted}
}

// projectedBlock is the model-visible form of one content block.
//
// It is deliberately not ContentBlock: a request carries a description of what the
// block held, never the block's bytes.
type projectedBlock struct {
	Type     ContentType `json:"type"`
	Text     string      `json:"text,omitempty"`
	MIMEType string      `json:"mime_type,omitempty"`
	Bytes    int         `json:"bytes,omitempty"`
	SHA256   string      `json:"sha256,omitempty"`
	URI      string      `json:"uri,omitempty"`
	Name     string      `json:"name,omitempty"`
	Note     string      `json:"note,omitempty"`
}

// projectContentBlock projects one block. keep is false when the block carries
// nothing the model can use at all; omitted is true when its payload was left out
// of the projection while its description was kept.
func projectContentBlock(block ContentBlock, repeated map[string]struct{}) (projected projectedBlock, keep, omitted bool) {
	switch block.Type {
	case ContentText:
		text := block.Text
		if trimmed := strings.TrimSpace(text); trimmed != "" {
			if _, duplicate := repeated[trimmed]; duplicate {
				return projectedBlock{}, false, false
			}
		}
		text, truncated := boundedText(text, MaxProjectedTextBlockBytes)
		return projectedBlock{Type: ContentText, Text: text}, text != "", truncated
	case ContentImage, ContentAudio:
		return projectedBlock{
			Type: block.Type, MIMEType: block.MIMEType, Bytes: len(block.Data), SHA256: digest(block.Data),
			Note: "binary content is not sent as text; it is stored with the session and can be read from there",
		}, true, true
	case ContentEmbeddedResource:
		projected = projectedBlock{Type: ContentEmbeddedResource, URI: block.URI, MIMEType: block.MIMEType}
		if block.Resource != nil {
			projected.URI = block.Resource.URI
			projected.MIMEType = block.Resource.MIMEType
			if block.Resource.Text != "" {
				text, truncated := boundedText(block.Resource.Text, MaxProjectedTextBlockBytes)
				projected.Text, omitted = text, truncated
			}
			if len(block.Resource.Blob) > 0 {
				projected.Bytes, projected.SHA256 = len(block.Resource.Blob), digest(block.Resource.Blob)
				projected.Note = "binary content is not sent as text; it is stored with the session and can be read from there"
				omitted = true
			}
		}
		return projected, projected.URI != "" || projected.Text != "", omitted
	case ContentResourceLink:
		return projectedBlock{
			Type: ContentResourceLink, URI: block.URI, Name: block.Name,
			MIMEType: block.MIMEType, Bytes: int(derefSize(block.Size)),
		}, true, false
	default:
		// An unrecognized block is described rather than inlined: the model is told
		// that something was returned and of what type.
		return projectedBlock{Type: block.Type, Note: "unknown content block type"}, true, true
	}
}

// projectStructuredContent bounds the structured content of a result. Content that
// does not fit is replaced whole, so the projection stays valid JSON whatever the
// stored document looked like.
func projectStructuredContent(raw json.RawMessage) (json.RawMessage, bool) {
	if len(raw) == 0 {
		return nil, false
	}
	if len(raw) <= MaxProjectedStructuredBytes && json.Valid(raw) {
		return raw, false
	}
	replacement := struct {
		Truncated bool   `json:"truncated"`
		Bytes     int    `json:"bytes"`
		SHA256    string `json:"sha256"`
		Note      string `json:"note"`
	}{
		Truncated: true, Bytes: len(raw), SHA256: digest(raw),
		Note: "the structured content did not fit the request budget; the full value is stored with the session",
	}
	encoded, err := json.Marshal(replacement)
	if err != nil {
		return nil, true
	}
	return encoded, true
}

// boundedText bounds one text value, cutting on a rune boundary and saying that it
// was cut.
func boundedText(value string, limit int) (string, bool) {
	if limit <= 0 || len(value) <= limit {
		return value, false
	}
	marker := "\n[content truncated]"
	if limit <= len(marker) {
		return marker[:limit], true
	}
	end := limit - len(marker)
	for end > 0 && !isRuneStart(value[end]) {
		end--
	}
	return value[:end] + marker, true
}

func isRuneStart(value byte) bool {
	return value&0xC0 != 0x80
}

func digest(data []byte) string {
	if len(data) == 0 {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func derefSize(size *int64) int64 {
	if size == nil {
		return 0
	}
	return *size
}
