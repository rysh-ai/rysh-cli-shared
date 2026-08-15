// SPDX-License-Identifier: Apache-2.0

package provider

import "testing"

func TestDefaultMaxTokensForModel(t *testing.T) {
	cases := []struct {
		model string
		want  int
	}{
		{"claude-sonnet-4-20250514", 16000},
		{"claude-sonnet-4-5-20250929", 16000},
		{"claude-sonnet-5", 16000},
		{"claude-3-5-sonnet-latest", 16000},
		{"claude-opus-4-8", 16000},
		{"claude-opus-4-1-20250805", 16000},
		{"claude-fable-5", 16000},
		{"claude-haiku-4-5-20251001", 8192},
		{"claude-3-5-haiku-20241022", 8192},
		{"some-future-model", 8192},
		{"", 8192},
	}
	for _, c := range cases {
		if got := DefaultMaxTokensForModel(c.model); got != c.want {
			t.Errorf("DefaultMaxTokensForModel(%q) = %d, want %d", c.model, got, c.want)
		}
	}
}
