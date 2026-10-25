package llm

import (
	"encoding/base64"
	"encoding/json"
	"testing"
)

func testImageMessage() Message {
	return Message{
		Role: "tool", ToolCallID: "call-1", Content: "Read image pixel.png",
		Images: []Image{{MediaType: "image/png", Data: []byte{1, 2, 3}, Name: "pixel.png"}},
	}
}

func TestOpenAIEncodesToolImages(t *testing.T) {
	messages := openAIMessages([]Message{testImageMessage()})
	encoded, _ := json.Marshal(messages)
	var decoded []map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	content, ok := decoded[0]["content"].([]any)
	if !ok || len(content) != 2 {
		t.Fatalf("content = %#v", decoded[0]["content"])
	}
	image := content[1].(map[string]any)
	if image["type"] != "image_url" {
		t.Fatalf("image block = %#v", image)
	}
	url := image["image_url"].(map[string]any)["url"].(string)
	want := "data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte{1, 2, 3})
	if url != want {
		t.Fatalf("image url = %q, want %q", url, want)
	}
}

func TestAnthropicEncodesToolImages(t *testing.T) {
	_, messages := anthropicMessages([]Message{testImageMessage()})
	encoded, _ := json.Marshal(messages)
	var decoded []map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	blocks := decoded[0]["content"].([]any)
	result := blocks[0].(map[string]any)
	content := result["content"].([]any)
	image := content[1].(map[string]any)
	source := image["source"].(map[string]any)
	if source["type"] != "base64" || source["media_type"] != "image/png" || source["data"] != base64.StdEncoding.EncodeToString([]byte{1, 2, 3}) {
		t.Fatalf("image source = %#v", source)
	}
}

func TestResponsesEncodesToolImages(t *testing.T) {
	body := encodeResponsesRequest(Request{Model: "m", Messages: []Message{testImageMessage()}})
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatal(err)
	}
	input := decoded["input"].([]any)
	output := input[0].(map[string]any)["output"].([]any)
	if len(output) != 2 {
		t.Fatalf("output = %#v", output)
	}
	image := output[1].(map[string]any)
	if image["type"] != "input_image" || image["image_url"] != "data:image/png;base64,"+base64.StdEncoding.EncodeToString([]byte{1, 2, 3}) {
		t.Fatalf("image block = %#v", image)
	}
}
