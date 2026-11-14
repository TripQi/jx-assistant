package main

import (
	"strings"
	"testing"
)

func TestP1F25CompletionReasons(t *testing.T) {
	for _, protocol := range []string{"chat", "responses", "anthropic"} {
		t.Run(protocol, func(t *testing.T) {
			var got StreamCompletion
			err := protocolOf(protocol).parse(strings.NewReader(completionFixture(protocol, "truncated")), StreamCallbacks{OnCompletion: func(c StreamCompletion) { got = c }})
			want := map[string]string{"chat": "length", "responses": "max_output_tokens", "anthropic": "max_tokens"}[protocol]
			t.Logf("completion=%+v err=%v", got, err)
			if got.Reason != want || !got.Ended {
				t.Fatalf("completion reason lost: %+v", got)
			}
		})
	}
}
