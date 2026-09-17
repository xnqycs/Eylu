package protocol

import (
	"encoding/json"
	"strings"
	"testing"
)

// A result that only carries text projects to that text, which is what every
// driver sent before the projection existed.
func TestATextOnlyResultProjectsToItsText(t *testing.T) {
	result := ToolResult{CallID: "call-1", Content: "the answer"}
	if got := ToolResultText(result); got != "the answer" {
		t.Fatalf("projection = %q", got)
	}
	if projection := ProjectToolResult(result); projection.Omitted {
		t.Fatal("a text-only result was reported as incomplete")
	}
}

// One answer must not be sent twice: a text block that repeats the result's own
// text is dropped.
func TestATextBlockRepeatingTheResultTextIsDropped(t *testing.T) {
	result := ToolResult{
		CallID: "call-1", Content: "hello",
		ContentBlocks: []ContentBlock{{Type: ContentText, Text: "hello"}},
	}
	if got := ToolResultText(result); got != "hello" {
		t.Fatalf("projection = %q, want the text alone", got)
	}
	// A block that says something else is kept.
	result.ContentBlocks = append(result.ContentBlocks, ContentBlock{Type: ContentText, Text: "and more"})
	projected := ToolResultText(result)
	if !strings.Contains(projected, "and more") || strings.Count(projected, "hello") != 1 {
		t.Fatalf("projection = %q", projected)
	}
}

// Binary content is described, never inlined: a request carries a reference with
// the media type, the byte count and a digest, not base64 in a text field.
func TestBinaryContentIsDescribedRatherThanInlined(t *testing.T) {
	payload := []byte(strings.Repeat("A", 1<<20))
	result := ToolResult{
		CallID: "call-1", Content: "",
		ContentBlocks: []ContentBlock{
			{Type: ContentImage, MIMEType: "image/png", Data: payload},
			{Type: ContentAudio, MIMEType: "audio/wav", Data: []byte{1, 2, 3}},
		},
	}
	projection := ProjectToolResult(result)
	if !projection.Omitted {
		t.Fatal("binary content was carried without being reported as left out")
	}
	if len(projection.Text) > 1024 {
		t.Fatalf("the projection inlined the binary content: %d bytes", len(projection.Text))
	}
	var decoded struct {
		Blocks []struct {
			Type   string `json:"type"`
			Bytes  int    `json:"bytes"`
			SHA256 string `json:"sha256"`
			Note   string `json:"note"`
		} `json:"content_blocks"`
	}
	if err := json.Unmarshal([]byte(projection.Text), &decoded); err != nil {
		t.Fatalf("the projection is not valid JSON: %v (%q)", err, projection.Text)
	}
	if len(decoded.Blocks) != 2 || decoded.Blocks[0].Bytes != len(payload) || decoded.Blocks[0].SHA256 == "" || decoded.Blocks[0].Note == "" {
		t.Fatalf("blocks = %#v", decoded.Blocks)
	}
	if strings.Contains(projection.Text, "AAAA") {
		t.Fatal("the binary content reached the text field")
	}
}

// Structured content that does not fit the bound is replaced whole, so the
// projection stays valid JSON whatever the stored document looked like.
func TestOversizedStructuredContentIsReplacedByValidJSON(t *testing.T) {
	deep := make(map[string]any, 64)
	for index := 0; index < 64; index++ {
		deep[strings.Repeat("k", index+1)] = strings.Repeat("v", 512)
	}
	encoded, err := json.Marshal(deep)
	if err != nil {
		t.Fatal(err)
	}
	result := ToolResult{CallID: "call-1", Content: "summary", StructuredContent: encoded}
	projection := ProjectToolResult(result)
	if !projection.Omitted {
		t.Fatal("oversized structured content was carried in full")
	}
	if len(projection.Text) > MaxProjectedStructuredBytes+1024 {
		t.Fatalf("the projection is %d bytes, want it bounded", len(projection.Text))
	}
	var envelope struct {
		Structured struct {
			Truncated bool   `json:"truncated"`
			Bytes     int    `json:"bytes"`
			SHA256    string `json:"sha256"`
			Note      string `json:"note"`
		} `json:"structured_content"`
	}
	if err := json.Unmarshal([]byte(projection.Text), &envelope); err != nil {
		t.Fatalf("the projection is not valid JSON: %v", err)
	}
	if !envelope.Structured.Truncated || envelope.Structured.Bytes != len(encoded) || envelope.Structured.SHA256 == "" {
		t.Fatalf("structured = %#v", envelope.Structured)
	}

	// Content that fits is carried unchanged and stays valid.
	small := ToolResult{CallID: "call-1", StructuredContent: json.RawMessage(`{"count":2}`)}
	smallProjection := ProjectToolResult(small)
	if smallProjection.Omitted || !json.Valid([]byte(smallProjection.Text)) || !strings.Contains(smallProjection.Text, `"count":2`) {
		t.Fatalf("small structured content = %q (omitted %t)", smallProjection.Text, smallProjection.Omitted)
	}
}

// An oversized text block is bounded on a rune boundary, and the request says it
// was cut rather than pretending the text was complete.
func TestAnOversizedTextBlockIsBoundedOnARuneBoundary(t *testing.T) {
	result := ToolResult{
		CallID:        "call-1",
		ContentBlocks: []ContentBlock{{Type: ContentText, Text: strings.Repeat("界", 8<<10)}},
	}
	projection := ProjectToolResult(result)
	if !projection.Omitted {
		t.Fatal("an oversized text block was carried in full")
	}
	if len(projection.Text) > MaxProjectedTextBlockBytes+1024 {
		t.Fatalf("the projection is %d bytes", len(projection.Text))
	}
	if !strings.Contains(projection.Text, "content truncated") {
		t.Fatalf("the projection does not say it was cut: %q", projection.Text)
	}
	if !json.Valid([]byte(projection.Text)) {
		t.Fatalf("the bounded projection is not valid JSON: %q", projection.Text)
	}
}

// More blocks than the bound allows are described up to the bound, and the
// omission is reported.
func TestTheProjectionBoundsHowManyBlocksItDescribes(t *testing.T) {
	blocks := make([]ContentBlock, 0, MaxProjectedBlocks+10)
	for index := 0; index < MaxProjectedBlocks+10; index++ {
		blocks = append(blocks, ContentBlock{Type: ContentResourceLink, URI: "https://example.test/" + strings.Repeat("x", index+1)})
	}
	projection := ProjectToolResult(ToolResult{CallID: "call-1", ContentBlocks: blocks})
	if !projection.Omitted {
		t.Fatal("the extra blocks were not reported as left out")
	}
	var decoded struct {
		Blocks []json.RawMessage `json:"content_blocks"`
	}
	if err := json.Unmarshal([]byte(projection.Text), &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Blocks) != MaxProjectedBlocks {
		t.Fatalf("blocks = %d, want %d", len(decoded.Blocks), MaxProjectedBlocks)
	}
}

// An MCP result carries the same answer twice: the legacy text field is the
// concatenation of its text blocks. Only one of the two is sent.
func TestTextCarriedByBothTheFieldAndTheBlocksIsSentOnce(t *testing.T) {
	result := ToolResult{
		CallID: "call-1", Content: "first\nsecond",
		ContentBlocks: []ContentBlock{
			{Type: ContentText, Text: "first"},
			{Type: ContentText, Text: "second"},
			{Type: ContentText, Text: "only in a block"},
		},
	}
	projection := ProjectToolResult(result)
	if strings.Count(projection.Text, "first") != 1 || strings.Count(projection.Text, "second") != 1 {
		t.Fatalf("the text was sent more than once: %q", projection.Text)
	}
	if !strings.Contains(projection.Text, "only in a block") {
		t.Fatalf("a block the text field does not carry was dropped: %q", projection.Text)
	}
}

// Projecting a result never changes the stored one.
func TestProjectionDoesNotMutateTheStoredResult(t *testing.T) {
	result := ToolResult{
		CallID: "call-1", Content: "text",
		ContentBlocks:     []ContentBlock{{Type: ContentImage, MIMEType: "image/png", Data: []byte{1, 2, 3}}},
		StructuredContent: json.RawMessage(`{"count":2}`),
	}
	before := result
	_ = ToolResultText(result)
	if len(result.ContentBlocks) != len(before.ContentBlocks) || string(result.StructuredContent) != string(before.StructuredContent) || result.Content != before.Content {
		t.Fatalf("the stored result was changed: %#v", result)
	}
}
