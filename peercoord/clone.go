// SPDX-License-Identifier: Apache-2.0

package peercoord

import (
	"maps"
	"slices"
)

// cloneStore preserves transactional isolation without a JSON encode/decode on
// every heartbeat. Every mutable map, pointer and nested slice is copied.
func cloneStore(src Store) Store {
	dst := src
	dst.PeerSecrets = maps.Clone(src.PeerSecrets)
	dst.ExecutionSecrets = maps.Clone(src.ExecutionSecrets)
	dst.State.Leadership.Standbys = slices.Clone(src.State.Leadership.Standbys)
	dst.State.Peers = maps.Clone(src.State.Peers)
	for id, p := range src.State.Peers {
		if p != nil {
			v := *p
			dst.State.Peers[id] = &v
		}
	}
	dst.State.Agents = maps.Clone(src.State.Agents)
	for id, a := range src.State.Agents {
		if a != nil {
			v := *a
			v.Projects = slices.Clone(a.Projects)
			v.Capabilities = slices.Clone(a.Capabilities)
			dst.State.Agents[id] = &v
		}
	}
	dst.State.Tasks = maps.Clone(src.State.Tasks)
	for id, t := range src.State.Tasks {
		if t == nil {
			continue
		}
		v := *t
		v.Dependencies = slices.Clone(t.Dependencies)
		v.Attempts = slices.Clone(t.Attempts)
		for i := range v.Attempts {
			at := &v.Attempts[i]
			at.Result = slices.Clone(at.Result)
			if at.StartedAt != nil {
				stamp := *at.StartedAt
				at.StartedAt = &stamp
			}
			if at.EndedAt != nil {
				stamp := *at.EndedAt
				at.EndedAt = &stamp
			}
		}
		dst.State.Tasks[id] = &v
	}
	return dst
}
