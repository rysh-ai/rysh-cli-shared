// SPDX-License-Identifier: Apache-2.0

package peercoord

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

func TestStoreCopyIsolatesNestedMutations(t *testing.T) {
	now := time.Now().UTC()
	src := NewStore()
	src.PeerSecrets["p"] = "peer-secret"
	src.ExecutionSecrets["e"] = "execution-secret"
	src.State.Leadership.Standbys = []string{"standby"}
	src.State.Peers["p"] = &Peer{ID: "p", State: "ready"}
	src.State.Agents["a"] = &Agent{ID: "a", Projects: []string{"math"}, Capabilities: []string{"proof"}}
	src.State.Tasks["t"] = &Task{ID: "t", Dependencies: []string{"prior"}, Attempts: []Attempt{{StartedAt: &now, EndedAt: &now, Result: json.RawMessage(`{"summary":"524"}`)}}}
	before, err := json.Marshal(src)
	if err != nil {
		t.Fatal(err)
	}
	dst := cloneStore(src)
	if !reflect.DeepEqual(src, dst) {
		t.Fatal("copy changed content")
	}
	dst.PeerSecrets["p"] = "changed"
	dst.ExecutionSecrets["e"] = "changed"
	dst.State.Leadership.Standbys[0] = "changed"
	dst.State.Peers["p"].State = "revoked"
	dst.State.Agents["a"].Projects[0] = "changed"
	dst.State.Agents["a"].Capabilities[0] = "changed"
	dst.State.Tasks["t"].Dependencies[0] = "changed"
	dst.State.Tasks["t"].Attempts[0].Result[0] = '['
	*dst.State.Tasks["t"].Attempts[0].StartedAt = time.Time{}
	*dst.State.Tasks["t"].Attempts[0].EndedAt = time.Time{}
	after, err := json.Marshal(src)
	if err != nil || string(before) != string(after) {
		t.Fatal("copy mutated its source")
	}
}

func BenchmarkStoreCopy(b *testing.B) {
	s := NewStore()
	for i := 0; i < 100; i++ {
		id := time.Unix(int64(i), 0).Format(time.RFC3339)
		s.State.Peers[id] = &Peer{ID: id, State: "ready"}
		s.State.Agents[id] = &Agent{ID: id, Projects: []string{"math"}, Capabilities: []string{"proof"}}
		s.State.Tasks[id] = &Task{ID: id, Attempts: []Attempt{{Result: json.RawMessage(`{"summary":"524","tests":["enumeration passed"]}`)}}}
		s.PeerSecrets[id] = "example-secret-hash"
	}
	b.Run("typed", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			_ = cloneStore(s)
		}
	})
	b.Run("json-baseline", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			data, _ := json.Marshal(s)
			var copy Store
			if err := json.Unmarshal(data, &copy); err != nil {
				b.Fatal(err)
			}
		}
	})
}
