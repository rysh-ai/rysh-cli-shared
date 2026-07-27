package tools

import (
	"context"
	"encoding/json"
	"sync"
)

// PageContext holds the content of the current browser tab, injected by the
// Chrome extension for each LLM request. The server stores one PageContext per
// pane ID in a sync.Map and clears it after the tool is invoked (single-use).
type PageContext struct {
	URL          string `json:"url"`
	Title        string `json:"title"`
	SelectedText string `json:"selected_text"`
	BodyText     string `json:"body_text"`
}

// PageContextTool allows the LLM to read the current browser tab's content.
// The Chrome plugin injects {url, title, selected_text, body_text} per-request.
// The server stores the context in a sync.Map keyed by pane ID and clears it
// after each tool invocation (single-use).
type PageContextTool struct {
	paneID string
	store  *sync.Map // keyed by paneID → PageContext
}

// NewPageContextTool creates a PageContextTool for the given pane.
// store is a shared *sync.Map maintained by BrowserPaneService.
func NewPageContextTool(paneID string, store *sync.Map) *PageContextTool {
	return &PageContextTool{
		paneID: paneID,
		store:  store,
	}
}

// SetPageContext stores a PageContext for a given pane ID. This is a
// package-level helper used by BrowserPaneService to inject context before
// the tool is invoked.
func SetPageContext(store *sync.Map, paneID string, ctx PageContext) {
	store.Store(paneID, ctx)
}

// Spec returns the tool specification for the LLM.
func (t *PageContextTool) Spec() ToolSpec {
	return ToolSpec{
		Name:             "page_context",
		Description:      "Get the content of the current browser tab including URL, title, selected text, and page body.",
		Parameters:       json.RawMessage(`{"type":"object","properties":{}}`),
		RequiresApproval: false,
	}
}

// RequiresApproval always returns false for page_context.
func (t *PageContextTool) RequiresApproval(_ json.RawMessage) bool {
	return false
}

// Execute returns the stored PageContext for this pane, then clears it (single-use).
// If no context is set, it returns a message indicating no context is available.
func (t *PageContextTool) Execute(_ context.Context, _ json.RawMessage) (*ToolOutput, error) {
	val, ok := t.store.LoadAndDelete(t.paneID)
	if !ok {
		return &ToolOutput{Content: "No page context available."}, nil
	}

	pc, ok := val.(PageContext)
	if !ok {
		return &ToolOutput{Content: "No page context available."}, nil
	}

	data, err := json.MarshalIndent(pc, "", "  ")
	if err != nil {
		return &ToolOutput{Content: "No page context available."}, nil
	}

	return &ToolOutput{Content: string(data)}, nil
}
