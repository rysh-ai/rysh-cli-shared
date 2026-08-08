package agentic

// turn_model.go — telling the active model which assistant turns are not its own.
//
// A pane's conversation outlives the model that started it. `##llm select`
// swaps the provider on a LIVE orchestrator, so the next model is handed a
// transcript whose assistant turns it never wrote — and the wire format gives
// it no way to tell: every assistant turn is role "assistant" regardless of who
// produced it. The observed failure is exactly what that predicts:
//
//	user      what model are you?
//	assistant I'm ChatGPT, an AI assistant ...     <- written by openai
//	          ## llm select -> anthropic
//	user      what model are you?
//	assistant I need to correct my previous answer — that was wrong. I'm Claude ...
//
// The second model did not lie; it read a stranger's answer in its own voice
// and dutifully corrected "itself".
//
// The fix is two halves. ConversationTurn.ProviderName/Model stamp every
// assistant turn with its producer (provider/agentic_provider.go). This file is
// the other half: at REQUEST-BUILD time it inserts a short synthetic note at
// each point where the producer changes, so the model reads an explicit
// hand-off instead of inferring ownership from role alone.
//
// Insertion happens on the outgoing copy (requestConversation), never on the
// stored transcript — the same discipline the ephemeral screenshot turn uses.
// The stored history stays exactly what each model actually said, so a note is
// never compounded across requests and the session JSON is unaffected.

import (
	"fmt"
	"strings"
	"time"

	"github.com/rysh-ai/rysh-cli-shared/provider"
)

// modelSwitchOrigin tags the synthetic turns this file inserts, matching the
// Origin convention of the other synthetic turns ("compaction",
// "ephemeral-screenshot").
const modelSwitchOrigin = "model-switch"

// turnAttribution is one producer identity.
type turnAttribution struct {
	providerName string
	model        string
}

func (a turnAttribution) empty() bool { return a.providerName == "" && a.model == "" }

func (a turnAttribution) label() string {
	return provider.ConversationTurn{ProviderName: a.providerName, Model: a.model}.Attribution()
}

// insertModelSwitchNotes returns conv with a synthetic note inserted after each
// run of assistant turns whose producer differs from what comes next — where
// "what comes next" is the following attributed assistant run, or the ACTIVE
// model when no attributed assistant turn follows.
//
// That placement is deliberate. The note lands immediately after the foreign
// turns it describes and BEFORE the user's next prompt, so the model reads the
// hand-off in chronological order rather than finding it appended after the
// question it is being asked to answer.
//
// Unattributed assistant turns (recorded before attribution existed, or by
// paths that do not stamp) are skipped rather than reported as a switch: a
// transcript that predates this cannot be split into runs honestly, and
// inventing a boundary would be worse than staying quiet.
//
// Returns conv unchanged — the same backing array, no copy — when there is
// nothing to say, which is every request in a session that never switched.
func insertModelSwitchNotes(conv []provider.ConversationTurn, active turnAttribution) []provider.ConversationTurn {
	boundaries := modelSwitchBoundaries(conv, active)
	if len(boundaries) == 0 {
		return conv
	}
	out := make([]provider.ConversationTurn, 0, len(conv)+len(boundaries))
	for i, turn := range conv {
		out = append(out, turn)
		if note, ok := boundaries[i]; ok {
			out = append(out, note)
		}
	}
	return out
}

// modelSwitchBoundaries maps "insert after this index" → the note to insert.
// Split out from the rebuild so the decision is testable on its own.
func modelSwitchBoundaries(conv []provider.ConversationTurn, active turnAttribution) map[int]provider.ConversationTurn {
	// Index every attributed assistant turn, in order.
	type attributed struct {
		idx  int
		attr turnAttribution
	}
	var runs []attributed
	for i, t := range conv {
		if t.Role != "assistant" {
			continue
		}
		attr := turnAttribution{providerName: t.ProviderName, model: t.Model}
		if attr.empty() {
			continue
		}
		runs = append(runs, attributed{idx: i, attr: attr})
	}
	if len(runs) == 0 {
		return nil
	}

	notes := map[int]provider.ConversationTurn{}
	for i, cur := range runs {
		// What follows this turn: the next attributed assistant turn, or the
		// active model when this is the last one.
		next := active
		if i+1 < len(runs) {
			next = runs[i+1].attr
		}
		if next == cur.attr || next.empty() {
			continue
		}
		notes[cur.idx] = modelSwitchNote(cur.attr, next)
	}
	if len(notes) == 0 {
		return nil
	}
	return notes
}

// modelSwitchNote builds the synthetic turn announcing one hand-off.
//
// Role "user" with Category TurnCategorySystem is the shape every other
// synthetic turn here uses (compaction, ephemeral screenshot): providers accept
// it in any position, and it keeps the note out of the assistant voice — a note
// written AS the assistant would be one more turn the model could mistake for
// its own.
func modelSwitchNote(from, to turnAttribution) provider.ConversationTurn {
	return provider.ConversationTurn{
		Role: "user",
		Content: fmt.Sprintf(
			"[rysh] Model change. The assistant turns above this line were produced by %s, "+
				"a different model — they are not yours. From here you are %s. "+
				"Do not claim, apologise for, or \"correct\" those earlier answers as your own; "+
				"if asked about them, say which model wrote them.",
			from.label(), to.label()),
		Category:    provider.TurnCategorySystem,
		Origin:      modelSwitchOrigin,
		Summary:     "model changed: " + from.label() + " → " + to.label(),
		TimestampMs: time.Now().UnixMilli(),
	}
}

// modelIdentityBlock tells the model which model it is.
//
// rysh knows this and the model does not: asked "which llm model are you?", a
// model can only recite what its training baked in, which is why the answer was
// "I'm Claude ... I don't have a way to verify the exact model checkpoint from
// inside this session" — and, on the openai seat, "the exact model identifier
// isn't exposed to me in this session". Both are true statements about a fact
// rysh had the whole time: it chose the provider, resolved the model id, and
// built the client.
//
// Worth stating precisely: this is the model rysh is CALLING, which is the
// honest claim available. A provider may serve a specific checkpoint behind an
// alias, so the block says what was requested rather than asserting a checkpoint
// rysh cannot see either.
func modelIdentityBlock(attr turnAttribution) string {
	return fmt.Sprintf(
		"[rysh] Model identity: rysh is sending this conversation to the %s provider, "+
			"model %q. That is authoritative — if you are asked which model you are, "+
			"answer with it instead of saying you cannot tell from inside the session. "+
			"(This is the model rysh requests; a provider may serve a specific "+
			"checkpoint under that name, so do not invent a version string beyond it.)",
		attr.providerName, attr.model)
}

// requestSystemPrompt returns the system prompt for the NEXT call: the stored
// prompt plus the identity of the model actually serving it.
//
// Composed per call rather than folded into o.systemPrompt at construction,
// because `##llm select` can change the model mid-conversation — a prompt built
// once would keep telling the new model it is the old one, which is a worse
// failure than not telling it at all.
func (o *OrchestratorActor) requestSystemPrompt() string {
	attr := o.activeAttribution()
	if attr.providerName == "" || attr.model == "" {
		// A provider that cannot name itself gets no claim made on its behalf.
		return o.systemPrompt
	}
	if strings.TrimSpace(o.systemPrompt) == "" {
		return modelIdentityBlock(attr)
	}
	return o.systemPrompt + "\n\n" + modelIdentityBlock(attr)
}

// activeAttribution is the provider/model serving the orchestrator's NEXT call.
// Model() is not on the AgenticProvider interface, so it is read through the
// same optional type assertion usageRecord uses; a provider that does not
// report one still attributes by provider name alone.
func (o *OrchestratorActor) activeAttribution() turnAttribution {
	if o.prov == nil {
		return turnAttribution{}
	}
	attr := turnAttribution{providerName: o.prov.Name()}
	if mp, ok := o.prov.(interface{ Model() string }); ok {
		attr.model = mp.Model()
	}
	return attr
}

// stampAttribution records the producing model on an assistant turn. Called at
// every site that appends one, so an unattributed assistant turn means "written
// before this existed" rather than "written by an unknown model".
func (o *OrchestratorActor) stampAttribution(t provider.ConversationTurn) provider.ConversationTurn {
	attr := o.activeAttribution()
	t.ProviderName, t.Model = attr.providerName, attr.model
	return t
}
