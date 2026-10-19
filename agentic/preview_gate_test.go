// SPDX-License-Identifier: Apache-2.0

package agentic

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/rysh-ai/rysh-cli-shared/provider"
	"github.com/rysh-ai/rysh-cli-shared/tools"
)

// fileTool records whether Execute (the write) ran. withPreview selects
// whether it also implements tools.Previewer.
type fileTool struct {
	executed, previewed int
	execErr             error
}

func (f *fileTool) Execute(context.Context, json.RawMessage) (*tools.ToolOutput, error) {
	f.executed++
	if f.execErr != nil {
		return nil, f.execErr
	}
	return &tools.ToolOutput{Content: "+written", Metadata: map[string]string{"file_path": "a.go"}}, nil
}
func (f *fileTool) Spec() tools.ToolSpec                  { return tools.ToolSpec{Name: "edit"} }
func (f *fileTool) RequiresApproval(json.RawMessage) bool { return true }

type previewingFileTool struct{ fileTool }

func (f *previewingFileTool) Preview(context.Context, json.RawMessage) (*tools.ToolOutput, error) {
	f.previewed++
	return &tools.ToolOutput{Content: "+preview", Metadata: map[string]string{"file_path": "a.go"}}, nil
}

// The approval prompt's diff must come from Preview, not Execute: before this
// fix the edit was written while the pane still said "waiting_approval", and a
// rejection left it on disk (design 024, "Article 021 live demonstration").
func TestPreviewFileChangeDoesNotExecuteBeforeApproval(t *testing.T) {
	o := &OrchestratorActor{ctx: context.Background()}
	tool := &previewingFileTool{}

	out, canPreview, err := o.previewFileChange(provider.ToolCallRequest{Name: "edit"}, tool)
	if err != nil || !canPreview || out.Content != "+preview" {
		t.Fatalf("got out=%+v canPreview=%v err=%v", out, canPreview, err)
	}
	if tool.executed != 0 {
		t.Fatalf("Execute (the write) ran %d time(s) before approval", tool.executed)
	}
}

func TestPreviewFileChangeFallsBackToExecuteForLegacyTools(t *testing.T) {
	o := &OrchestratorActor{ctx: context.Background()}
	tool := &fileTool{}
	if _, canPreview, _ := o.previewFileChange(provider.ToolCallRequest{Name: "edit"}, tool); canPreview || tool.executed != 1 {
		t.Fatalf("legacy tool: canPreview=%v executed=%d", canPreview, tool.executed)
	}
}

func TestApplyApprovedChangeExecutesOnceAndSurfacesErrors(t *testing.T) {
	tool := &previewingFileTool{}
	out := applyApprovedChange(context.Background(), tool, nil)
	if tool.executed != 1 || out.Content != "+written" {
		t.Fatalf("executed=%d out=%+v", tool.executed, out)
	}
	bad := &previewingFileTool{fileTool{execErr: errors.New("disk full")}}
	if out := applyApprovedChange(context.Background(), bad, nil); out.Error == "" {
		t.Fatalf("apply error was swallowed: %+v", out)
	}
}
