package chatgptweb

import (
	"encoding/json"
	"testing"
)

func TestEditableArtifactsPickTheNewestPrimaryAndZip(t *testing.T) {
	var conversation map[string]any
	json.Unmarshal([]byte(`{"mapping": {
		"a": {"message": {"id": "m1", "create_time": 1, "author": {"role": "user"}, "content": {"parts": ["sandbox:/mnt/data/ignored.pptx"]}}},
		"b": {"message": {"id": "m2", "create_time": 2, "author": {"role": "assistant"},
			"metadata": {"attachments": [{"id": "file_aaa", "name": "old.pptx", "mime_type": "application/vnd.ms-powerpoint"}]},
			"content": {"parts": ["draft"]}}},
		"c": {"message": {"id": "m3", "create_time": 3, "author": {"role": "assistant"},
			"content": {"parts": ["Done: [deck](sandbox:/mnt/data/final.pptx) and [parts](sandbox:/mnt/data/parts.zip)"]}}}
	}}`), &conversation)
	items := extractArtifacts(conversation, pptKind.exportRE)
	targets := pickTargets(items, pptKind)
	if len(targets) != 2 || targets[0].SandboxPath != "/mnt/data/final.pptx" || targets[0].MessageID != "m3" || targets[1].SandboxPath != "/mnt/data/parts.zip" {
		t.Fatalf("targets %+v (from %+v)", targets, items)
	}
	if pickTargets(items[:1], pptKind) != nil {
		t.Fatal("a primary without a zip is not done yet")
	}
}

func TestFileIDsSkipTheFileServiceScheme(t *testing.T) {
	if got := fileIDMatch("file-service://x file_00000000abc"); got != "file_00000000abc" {
		t.Fatalf("got %q", got)
	}
	if editablePrompt(EditablePSDPrompt, "  ") != EditablePSDPrompt || editablePrompt("A", "B") != "A"+editableUserSupplementHeading+"B" {
		t.Fatal("prompt")
	}
}
