// SPDX-License-Identifier: Apache-2.0

package toolpack

import (
	"testing"

	"github.com/rysh-ai/rysh-cli-shared/forge/ingest"
)

const planSpec = `{"openapi":"3.0.0","info":{"title":"Petstore","version":"1"},"servers":[{"url":"https://api.example.com"}],
"paths":{"/pets":{"get":{"operationId":"listPets","responses":{"200":{"description":"ok"}}},
"post":{"operationId":"createPet","responses":{"201":{"description":"created"}}}},
"/pets/{id}":{"get":{"operationId":"getPet","parameters":[{"name":"id","in":"path","required":true,"schema":{"type":"string"}}],"responses":{"200":{"description":"ok"}}},
"delete":{"operationId":"deletePet","parameters":[{"name":"id","in":"path","required":true,"schema":{"type":"string"}}],"responses":{"204":{"description":"gone"}}}}}}`

func TestPlanMatchesRegisterAndHonoursReadOnly(t *testing.T) {
	api, err := ingest.OpenAPI([]byte(planSpec))
	if err != nil {
		t.Fatal(err)
	}
	tp := Build(api, "pets_")

	names, mode := tp.Plan(api, Policy{ReadOnly: true, Prefix: "pets_"})
	if mode != ModeStatic {
		t.Fatalf("mode = %q, want static", mode)
	}
	if len(names) != 2 {
		t.Fatalf("read-only plan = %v, want the two GET operations", names)
	}
	for _, n := range names {
		if n == "pets_createPet" || n == "pets_deletePet" {
			t.Fatalf("read-only plan exposed a mutating tool: %v", names)
		}
	}

	all, _ := tp.Plan(api, Policy{Prefix: "pets_"})
	if len(all) != 4 {
		t.Fatalf("full plan = %v, want four tools", all)
	}

	// Above the threshold the plan turns dynamic: three meta-tools.
	dyn, dmode := tp.Plan(api, Policy{Prefix: "pets_", DynamicThreshold: 2})
	if dmode != ModeDynamic || len(dyn) != 3 {
		t.Fatalf("dynamic plan = %v (%q), want three meta-tools", dyn, dmode)
	}
	capped, _ := tp.Plan(api, Policy{Prefix: "pets_", MaxTools: 1, DynamicThreshold: 100})
	if len(capped) != 1 {
		t.Fatalf("capped plan = %v, want one tool", capped)
	}
}
