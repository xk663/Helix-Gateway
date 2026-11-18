package truncationfix

import (
	"fmt"
	"strings"
	"testing"
)

func TestReservedNameInProseAndToolMetadata(t *testing.T) {
	cfg := Config{Enabled: true, WrapperMode: "synthetic"}.Resolve()
	_, fix, _ := cfg.Prepare([]byte(`{"model":"qwen-mock","messages":[{"role":"user","content":"hello"}]}`))
	for _, name := range []string{ToolName, strings.Replace(ToolName, "_", `\u005f`, 1)} {
		content := fmt.Sprintf(`{"choices":[{"message":{"content":"the name %s is reserved"},"finish_reason":"stop"}]}`, name)
		if got, err := fix.Complete([]byte(content)); err != nil || string(got) != content {
			t.Fatalf("ordinary prose was altered or blocked: %s (%v)", got, err)
		}
		legacy := fmt.Sprintf(`{"choices":[{"message":{"function_call":{"name":"%s","arguments":"{}"}},"finish_reason":"function_call"}]}`, name)
		if _, err := fix.Complete([]byte(legacy)); err != ErrInvalidResponse {
			t.Fatalf("reserved name escaped in legacy metadata: %v", err)
		}
	}
}

func TestSyntheticOverlapAcrossUTF8StreamChunks(t *testing.T) {
	const shared = "一二三四五六七八九"
	const plain = "前言。" + shared
	const extra = "，从这里继续。"
	for _, streaming := range []bool{false, true} {
		t.Run(fmt.Sprint("stream_", streaming), func(t *testing.T) {
			cfg := Config{Enabled: true, WrapperMode: "synthetic"}.Resolve()
			_, fix, _ := cfg.Prepare([]byte(`{"model":"qwen-mock","messages":[{"role":"user","content":"hello"}]}`))
			var got string
			if !streaming {
				raw := fmt.Sprintf(`{"choices":[{"message":{"content":%q,"tool_calls":[{"function":{"name":%q,"arguments":%q}}]},"finish_reason":"tool_calls"}]}`,
					plain, ToolName, `{"content":"`+shared+extra+`"}`)
				result, err := fix.Complete([]byte(raw))
				if err != nil {
					t.Fatal(err)
				}
				got = asString(asObject(asObject(asArray(asObject(result)["choices"])[0])["message"])["content"])
			} else {
				stream := fix.Stream()
				for _, raw := range []string{
					fmt.Sprintf(`{"choices":[{"index":0,"delta":{"content":%q},"finish_reason":null}]}`, plain),
					fmt.Sprintf(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"name":%q,"arguments":%q}}]},"finish_reason":null}]}`,
						ToolName, `{"content":"一二三`),
					fmt.Sprintf(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":%q}}]},"finish_reason":null}]}`,
						`四五六七八九`+extra+`"}`),
					`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
				} {
					output := mustChunk(t, stream, []byte(raw))
					for _, choice := range asArray(asObject(output)["choices"]) {
						got += asString(asObject(asObject(choice)["delta"])["content"])
					}
				}
			}
			report := fix.Report()
			if got != plain+extra || report.DeduplicatedBytes != uint64(len(shared)) ||
				report.RecoveredBytes != uint64(len(extra)) || report.ToolHits != 1 {
				t.Fatalf("got %q, counters %+v", got, report)
			}
		})
	}
}

func TestFullRepeatAndLongSuffix(t *testing.T) {
	const sentence = "Hello world, this is a long enough sentence to test dedup properly."
	longReply := sentence + strings.Repeat("长句。", 180)
	suffix := strings.Repeat("界", 90) + "结尾"
	for _, tc := range []struct {
		name, plain, synthetic, want string
		cut                          int
	}{
		{"reported sentence", sentence, sentence, "", len(sentence)},
		{"full repeat beyond window", longReply, longReply, "", len(longReply)},
		{"full repeat followed by new text", longReply, longReply + "新增", "新增", len(longReply)},
		{"shorter copy of prefix", longReply, sentence, "", len(sentence)},
		{"tail beyond previous window", "前言" + suffix, suffix + "新增", "新增", len(suffix)},
		{"preserve diverging prefix", longReply, sentence + "不同结局", sentence + "不同结局", 0},
		{"preserve short repetition", "一二三四五六七", "一二三四五六七", "一二三四五六七", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var oneShot TextOverlap
			if text, cut := oneShot.Merge(tc.plain, tc.synthetic, true); text != tc.want || cut != tc.cut {
				t.Fatalf("single merge produced %q, cut %d", text, cut)
			}
			var fragmented TextOverlap
			var text strings.Builder
			var removed int
			for _, r := range tc.synthetic {
				part, cut := fragmented.Merge(tc.plain, string(r), false)
				text.WriteString(part)
				removed += cut
			}
			part, cut := fragmented.Merge(tc.plain, "", true)
			text.WriteString(part)
			if text.String() != tc.want || removed+cut != tc.cut {
				t.Fatalf("incremental merge produced %q, cut %d", &text, removed+cut)
			}
		})
	}
}
