// SPDX-License-Identifier: Apache-2.0

package msg

// RegisterAgenticCodecs registers all agentic message types with the given codec registry.
// This is called from DefaultCodecRegistry or can be called separately to extend an
// existing registry.
func RegisterAgenticCodecs(r *CodecRegistry) {
	r.Register(TagAgenticPrompt, "*msg.MsgAgenticPrompt", jsonDecoder[MsgAgenticPrompt]())
	r.Register(TagAgenticCancel, "*msg.MsgAgenticCancel", jsonDecoder[MsgAgenticCancel]())
	r.Register(TagAgenticContinue, "*msg.MsgAgenticContinue", jsonDecoder[MsgAgenticContinue]())
	r.Register(TagAgenticStep, "*msg.MsgAgenticStep", jsonDecoder[MsgAgenticStep]())
	r.Register(TagOrchestratorDone, "*msg.MsgOrchestratorDone", jsonDecoder[MsgOrchestratorDone]())
	r.Register(TagToolCall, "*msg.MsgToolCall", jsonDecoder[MsgToolCall]())
	r.Register(TagToolResult, "*msg.MsgToolResult", jsonDecoder[MsgToolResult]())
	r.Register(TagAgenticOutput, "*msg.MsgAgenticOutput", jsonDecoder[MsgAgenticOutput]())
	r.Register(TagAgenticStatus, "*msg.MsgAgenticStatus", jsonDecoder[MsgAgenticStatus]())
	r.Register(TagApprovalRequest, "*msg.MsgApprovalRequest", jsonDecoder[MsgApprovalRequest]())
	r.Register(TagApprovalResponse, "*msg.MsgApprovalResponse", jsonDecoder[MsgApprovalResponse]())
	r.Register(TagSpawnSubOrchestrator, "*msg.MsgSpawnSubOrchestrator", jsonDecoder[MsgSpawnSubOrchestrator]())
	r.Register(TagSubOrchestratorResult, "*msg.MsgSubOrchestratorResult", jsonDecoder[MsgSubOrchestratorResult]())
	r.Register(TagGetConversationHistory, "*msg.MsgGetConversationHistory", jsonDecoder[MsgGetConversationHistory]())
	r.Register(TagConversationHistoryReply, "*msg.MsgConversationHistoryReply", jsonDecoder[MsgConversationHistoryReply]())
	r.Register(TagRestoreConversation, "*msg.MsgRestoreConversation", jsonDecoder[MsgRestoreConversation]())
	r.Register(TagGetSessionMemory, "*msg.MsgGetSessionMemory", jsonDecoder[MsgGetSessionMemory]())
	r.Register(TagSessionMemoryReply, "*msg.MsgSessionMemoryReply", jsonDecoder[MsgSessionMemoryReply]())
	r.Register(TagSessionMemoryReplace, "*msg.MsgSessionMemoryReplace", jsonDecoder[MsgSessionMemoryReplace]())
	r.Register(TagSetGroundingMode, "*msg.MsgSetGroundingMode", jsonDecoder[MsgSetGroundingMode]())
	r.Register(TagGetGroundingState, "*msg.MsgGetGroundingState", jsonDecoder[MsgGetGroundingState]())
	r.Register(TagGroundingStateReply, "*msg.MsgGroundingStateReply", jsonDecoder[MsgGroundingStateReply]())
	r.Register(TagSetChatOutputPane, "*msg.MsgSetChatOutputPane", jsonDecoder[MsgSetChatOutputPane]())

	// Approval flow among panes
	r.Register(TagCreateApprovalPane, "*msg.MsgCreateApprovalPane", jsonDecoder[MsgCreateApprovalPane]())
	r.Register(TagDestroyApprovalPane, "*msg.MsgDestroyApprovalPane", jsonDecoder[MsgDestroyApprovalPane]())
	r.Register(TagPaneSetApprovalPaneGroups, "*msg.MsgPaneSetApprovalPaneGroups", jsonDecoder[MsgPaneSetApprovalPaneGroups]())

	// Memory state injection
	r.Register(TagMemoryStateUpdate, "*msg.MsgMemoryStateUpdate", jsonDecoder[MsgMemoryStateUpdate]())

	// Prompt hot-reload broadcast (follow-up 2b)
	r.Register(TagReloadPrompts, "*msg.MsgReloadPrompts", jsonDecoder[MsgReloadPrompts]())

	// Auto-continue run budget (##web auto long runs)
	r.Register(TagSetRunBudget, "*msg.MsgSetRunBudget", jsonDecoder[MsgSetRunBudget]())
	r.Register(TagGetRunStatus, "*msg.MsgGetRunStatus", jsonDecoder[MsgGetRunStatus]())
	r.Register(TagRunStatusReply, "*msg.MsgRunStatusReply", jsonDecoder[MsgRunStatusReply]())
}
