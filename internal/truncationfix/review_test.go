package truncationfix

import (
	"strings"
	"testing"
)

func TestContentStreamsAfterLongNestedFields(t *testing.T) {
	var a arguments
	head := `{"metadata":{"content":"ignore this","tags":["quoted \"content\"","` +
		strings.Repeat("x", 4000) + `"]},"unused": true,"con\u0074ent": "`
	for i := 0; i < len(head); i++ {
		if text := a.feed(head[i:i+1], 8192); text != "" {
			t.Fatalf("recovered text from an earlier field: %q", text)
		}
	}
	runeText := "雪"
	if got := a.feed(runeText[:1], 8192); got != "" {
		t.Fatalf("emitted a partial UTF-8 rune: %q", got)
	}
	if got := a.feed(runeText[1:]+"山", 8192); got != "雪山" {
		t.Fatalf("later content was buffered instead of streamed: %q", got)
	}
	if got := a.feed("\"}", 8192); got != "" {
		t.Fatalf("emitted the closing delimiter: %q", got)
	}
	if tail, complete := a.finish("conservative"); tail != "" || !complete {
		t.Fatalf("failed final validation: %q %v", tail, complete)
	}
}

func TestVariableLengthFences(t *testing.T) {
	const json = `{"content":"雪"}`
	for _, fence := range []string{strings.Repeat("\x60", 4), strings.Repeat("~", 5)} {
		for _, body := range []string{
			fence + "json\r\n" + json + "\r\n " + fence + " \r\n",
			fence + "json\n" + json + "\n" + fence + fence[:1],
			fence + "json\n" + json,
		} {
			if got, ok := extractContent(body, "moderate"); !ok || got != "雪" {
				t.Errorf("failed to extract fenced content %q: %q %v", body, got, ok)
			}
			if _, ok := extractContent(body, "conservative"); ok {
				t.Fatal("conservative parsed a Markdown wrapper")
			}
		}
		if _, ok := extractContent(fence+"json\n"+json+"\n"+fence[:3], "moderate"); ok {
			t.Fatal("a shorter fence unexpectedly closed a longer fence")
		}
	}
}

func TestCompletionPreservesOrdinaryPreamble(t *testing.T) {
	req := newRequest(t)
	message := object{"content": encode("前言："), "tool_calls": encode([]object{
		tool(req.name, `{"content":"这里是正文。"}`, 0),
	})}
	original := encode(object{"choices": encode([]object{{"message": encode(message), "finish_reason": encode("tool_calls")}})})
	out, err := req.Complete(original)
	if err != nil {
		t.Fatal(err)
	}
	choice := readChoice(out)
	result := asObject(choice["message"])
	if got := asString(result["content"]); got != "前言：这里是正文。" ||
		len(asArray(result["tool_calls"])) != 0 || req.Report().RecoveredBytes != uint64(len("这里是正文。")) {
		t.Fatalf("incorrect content or statistics: %s %+v", out, req.Report())
	}
}
