package chatgptweb

import (
	"reflect"
	"testing"
)

func TestCodexEventsAndImages(t *testing.T) {
	sse := []byte("event: response.created\ndata: {\"type\":\"response.created\"}\n\n" +
		"data: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"image_generation_call\",\"result\":\"data:image/png;base64,QUJD\"}}\n\n" +
		"data: [DONE]\n\n")
	events := codexEvents("text/event-stream", sse)
	if len(events) != 2 {
		t.Fatalf("events %v", events)
	}
	if got := codexResponseImages(events); !reflect.DeepEqual(got, []string{"QUJD"}) {
		t.Fatalf("images %v", got)
	}
	single := codexEvents("application/json", []byte(`{"output":[{"type":"image_generation_call","result":"WFla"}]}`))
	if got := codexResponseImages(single); !reflect.DeepEqual(got, []string{"WFla"}) {
		t.Fatalf("json body images %v", got)
	}
	if got := codexResponseImages(codexEvents("text/event-stream", []byte("data: {\"type\":\"image_generation_call\",\"result\":\"  \"}\n\n"))); len(got) != 0 {
		t.Fatalf("blank result must be ignored: %v", got)
	}
}

func TestCodexImageInput(t *testing.T) {
	input := codexImageInput("draw", []string{"QUJD", "data:image/jpeg;base64,WFla"})
	content := input[0].(map[string]any)["content"].([]any)
	if content[1].(map[string]any)["image_url"] != "data:image/png;base64,QUJD" || content[2].(map[string]any)["image_url"] != "data:image/jpeg;base64,WFla" {
		t.Fatalf("content %v", content)
	}
}
