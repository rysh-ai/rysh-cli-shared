// SPDX-License-Identifier: Apache-2.0

package toolpack

import (
	"fmt"
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-shared/forge/ingest"
)

// opsSpec builds an OpenAPI document with n GET operations.
func opsSpec(n int) []byte {
	paths := make([]string, n)
	for i := range paths {
		paths[i] = fmt.Sprintf(`"/r%d":{"get":{"operationId":"op%d","responses":{"200":{"description":"ok"}}}}`, i, i)
	}
	return []byte(`{"openapi":"3.0.0","info":{"title":"Big","version":"1"},"servers":[{"url":"https://api.example.com"}],"paths":{` +
		strings.Join(paths, ",") + `}}`)
}

// TestAutoModeSwitchesOnSize pins the auto exposure mode. `forge add` stores
// the CLI default "auto" in integrations.json, while ModeAuto is the empty
// string; before the fix "auto" matched neither branch and every spec
// registered static, one tool per op.
func TestAutoModeSwitchesOnSize(t *testing.T) {
	big, err := ingest.OpenAPI(opsSpec(120))
	if err != nil {
		t.Fatal(err)
	}
	small, err := ingest.OpenAPI(opsSpec(5))
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name      string
		api       int // 120 or 5 ops
		mode      ExposureMode
		wantMode  ExposureMode
		wantTools int
	}{
		{"auto string, large spec", 120, "auto", ModeDynamic, 3},
		{"empty mode, large spec", 120, ModeAuto, ModeDynamic, 3},
		{"auto string, small spec", 5, "auto", ModeStatic, 5},
		{"empty mode, small spec", 5, ModeAuto, ModeStatic, 5},
		{"explicit static, large spec", 120, ModeStatic, ModeStatic, 120},
		{"explicit dynamic, small spec", 5, ModeDynamic, ModeDynamic, 3},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			api := big
			if c.api == 5 {
				api = small
			}
			tp := Build(api, "big_")
			names, mode := tp.Plan(api, Policy{Mode: c.mode, Prefix: "big_"})
			if mode != c.wantMode || len(names) != c.wantTools {
				t.Fatalf("mode %q: got %d tool(s) (exposure: %s), want %d (exposure: %s)",
					c.mode, len(names), mode, c.wantTools, c.wantMode)
			}
		})
	}
}
