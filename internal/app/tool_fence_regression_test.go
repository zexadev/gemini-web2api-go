package app

import (
	"fmt"
	"testing"
)

// Invalid tool JSON must not turn an embedded Markdown fence into the end of
// the tool block, exposing the rest of its arguments as assistant text.
func TestToolFenceGateHoldsMalformedArguments(t *testing.T) {
	const preamble = "Preparing the document.\n"
	const mermaid = "```mermaid\\nflowchart LR\\nA --> B\\n```"
	cases := []struct {
		name string
		body string
	}{
		{
			name: "invalid_value_before_nested_mermaid",
			body: `{"name":write,"arguments":{"content":"` + mermaid + `"}}`,
		},
		{
			name: "unescaped_quote_before_nested_mermaid",
			body: `{"name":"write","arguments":{"path":"broken".md","content":"` + mermaid + `"}}`,
		},
		{
			name: "raw_newline_in_argument_string",
			body: `{"name":"write","arguments":{"content":"Intro` + "\n" + mermaid + `"}}`,
		},
		{
			name: "garbage_after_complete_object",
			body: `{"name":"write","arguments":{}} trailing garbage`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			full := preamble + "```tool_call\n" + tc.body + "\n```\nThis must stay buffered."
			check := func(t *testing.T, chunks []string) {
				t.Helper()
				if got := feed(t, chunks); got != preamble {
					t.Fatalf("malformed tool block leaked: got %q, want only %q", got, preamble)
				}
			}
			t.Run("whole_response", func(t *testing.T) { check(t, []string{full}) })
			// One-byte chunks split every JSON token and every fence marker;
			// wider chunks cover several boundaries within one response.
			for _, width := range []int{1, 7, 17} {
				t.Run(fmt.Sprintf("chunks_of_%d_bytes", width), func(t *testing.T) {
					var chunks []string
					for i := 0; i < len(full); i += width {
						chunks = append(chunks, full[i:min(i+width, len(full))])
					}
					check(t, chunks)
				})
			}
			t.Run("every_two_chunk_boundary", func(t *testing.T) {
				for split := 1; split < len(full); split++ {
					check(t, []string{full[:split], full[split:]})
				}
			})
		})
	}
}
