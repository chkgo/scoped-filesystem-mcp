package server

import (
	"context"
	"fmt"
	"testing"

	"github.com/chkgo/scoped-filesystem-mcp/internal/filesystem"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestMutationRecoveryOutputRetainsCompleteOutcome(t *testing.T) {
	for _, state := range []filesystem.TargetState{filesystem.TargetProposalApplied, filesystem.TargetCurrentRestored, filesystem.TargetUnknown} {
		t.Run(string(state), func(t *testing.T) {
			srv := mcp.NewServer(&mcp.Implementation{Name: "recovery-test", Version: "1"}, nil)
			mcp.AddTool(srv, mutationTool[WriteOutput]("edit_file", "Exercise shared write error mapping.", true), func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, any, error) {
				return failed(fmt.Errorf("wrapped recovery: %w", &filesystem.RecoveryError{Outcome: filesystem.RecoveryOutcome{TargetState: state, RecoveryPath: ".recovery-note", RecoveryContent: "third version", RecoveryRevision: "sha256:third", RecoveryContentAvailable: true}}))
			})
			a, b := mcp.NewInMemoryTransports()
			ss, err := srv.Connect(context.Background(), a, nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = ss.Close() })
			prompts := 0
			opts := choose("overwrite")
			opts.ElicitationHandler = func(context.Context, *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
				prompts++
				return acceptance("overwrite"), nil
			}
			cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, opts).Connect(context.Background(), b, nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = cs.Close() })
			result := callTool(t, cs, "edit_file", map[string]any{})
			if !result.IsError || result.NeedsInput() || prompts != 0 || structuredString(t, result, "code") != "recovery_required" || structuredString(t, result, "recovery.target_state") != string(state) || structuredString(t, result, "recovery.recovery_path") != ".recovery-note" || structuredString(t, result, "recovery.recovery_content") != "third version" || structuredString(t, result, "recovery.recovery_revision") != "sha256:third" || !structuredBool(t, result, "recovery.content_available") {
				t.Fatalf("recovery = %#v, prompts = %d", result, prompts)
			}
			assertStructuredOutputMatchesSchema(t, cs, "edit_file", result)
		})
	}
}
