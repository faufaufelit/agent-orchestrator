package chat

import (
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestSettingsFromConfigOptionsKeepsClaudeModelAndEffortAcrossRestart(t *testing.T) {
	settings, changed := settingsFromConfigOptions(domain.ConversationSettings{
		ApprovalMode: domain.PermissionModeBypassPermissions,
	}, []ports.ChatConfigOption{
		{ID: "model", Category: "model", Current: ports.ChatConfigOptionValue{Select: "sonnet"}},
		{ID: "effort", Category: "thought_level", Current: ports.ChatConfigOptionValue{Select: "high"}},
	}, "effort")
	if !changed {
		t.Fatal("settings should change")
	}
	if settings.Model != "sonnet" || settings.ReasoningEffort != "high" || settings.ApprovalMode != domain.PermissionModeBypassPermissions {
		t.Fatalf("settings = %+v, want model and effort while preserving approval", settings)
	}
}

func TestPermissionConfigOptions(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  domain.PermissionMode
	}{{"manual", domain.PermissionModeDefault}, {"default", domain.PermissionModeDefault}, {"acceptEdits", domain.PermissionModeAcceptEdits}, {"auto", domain.PermissionModeAuto}, {"bypassPermissions", domain.PermissionModeBypassPermissions}, {"dontAsk", ""}, {"plan", ""}, {"custom", ""}} {
		input := []ports.ChatConfigOption{{ID: "mode", Current: ports.ChatConfigOptionValue{Select: tc.value}, Choices: []ports.ChatConfigOptionChoice{{Value: tc.value}}}}
		got := permissionConfigOptions(domain.HarnessClaudeCode, input)
		if got[0].Choices[0].PermissionMode != tc.want {
			t.Fatalf("%s mapping = %q", tc.value, got[0].Choices[0].PermissionMode)
		}
		settings, _ := settingsFromConfigOptions(domain.ConversationSettings{ApprovalMode: domain.PermissionModeAuto}, got, "mode")
		want := tc.want
		if want == "" {
			want = domain.PermissionModeAuto
		}
		if settings.ApprovalMode != want {
			t.Fatalf("%s settings=%q", tc.value, settings.ApprovalMode)
		}
		if input[0].Choices[0].PermissionMode != "" {
			t.Fatal("mutated provider catalog")
		}
		if other := permissionConfigOptions(domain.HarnessOpenCode, input); other[0].Choices[0].PermissionMode != "" {
			t.Fatal("mapped unknown provider")
		}
		input[0].ID = "model"
		if model := permissionConfigOptions(domain.HarnessClaudeCode, input); model[0].Choices[0].PermissionMode != "" {
			t.Fatal("mapped model choice")
		}
	}
}

func TestSettingsFromConfigOptionsPreservesEffortOnUnrelatedChange(t *testing.T) {
	// The provider reverts its session effort to default between turns. An
	// approval-only change must not clobber the stored pick from that
	// revert, or users re-pick effort for every message.
	settings, changed := settingsFromConfigOptions(domain.ConversationSettings{
		Model:           "opencode-go/deepseek-v4.1-flash",
		ReasoningEffort: "max",
		ApprovalMode:    domain.PermissionModeAuto,
	}, []ports.ChatConfigOption{
		{ID: "effort", Category: "thought_level", Current: ports.ChatConfigOptionValue{Select: "default"}},
		{ID: "mode", Category: "mode", Current: ports.ChatConfigOptionValue{Select: "auto"},
			Choices: []ports.ChatConfigOptionChoice{{Value: "auto", PermissionMode: domain.PermissionModeAuto}}},
	}, "mode")
	if changed {
		t.Fatalf("unrelated change must not touch settings, got %+v", settings)
	}
	if settings.ReasoningEffort != "max" {
		t.Fatalf("effort = %q, want stored pick %q", settings.ReasoningEffort, "max")
	}
}

func TestSettingsFromConfigOptionsFollowsEffortAndModelChanges(t *testing.T) {
	stored := domain.ConversationSettings{Model: "m1", ReasoningEffort: "max"}
	options := []ports.ChatConfigOption{
		{ID: "model", Category: "model", Current: ports.ChatConfigOptionValue{Select: "m1"}},
		{ID: "effort", Category: "thought_level", Current: ports.ChatConfigOptionValue{Select: "high"}},
	}
	// An explicit effort pick moves the stored value, even to empty (clear).
	if settings, _ := settingsFromConfigOptions(stored, options, "effort"); settings.ReasoningEffort != "high" {
		t.Fatalf("effort pick not applied: %+v", settings)
	}
	cleared := []ports.ChatConfigOption{
		{ID: "effort", Category: "thought_level", Current: ports.ChatConfigOptionValue{}},
	}
	if settings, _ := settingsFromConfigOptions(stored, cleared, "effort"); settings.ReasoningEffort != "" {
		t.Fatalf("effort clear not applied: %+v", settings)
	}
	// A model change resets provider variants: follow the live catalog even
	// back to default.
	reverted := []ports.ChatConfigOption{
		{ID: "model", Category: "model", Current: ports.ChatConfigOptionValue{Select: "m2"}},
		{ID: "effort", Category: "thought_level", Current: ports.ChatConfigOptionValue{Select: "default"}},
	}
	if settings, _ := settingsFromConfigOptions(stored, reverted, "model"); settings.ReasoningEffort != "default" || settings.Model != "m2" {
		t.Fatalf("model change did not follow live catalog: %+v", settings)
	}
}

func TestSettingsFromConfigOptionsClearsEffortWhenCapabilityGone(t *testing.T) {
	stored := domain.ConversationSettings{Model: "m1", ReasoningEffort: "max"}
	options := []ports.ChatConfigOption{
		{ID: "mode", Category: "mode", Current: ports.ChatConfigOptionValue{Select: "auto"}},
	}
	settings, changed := settingsFromConfigOptions(stored, options, "mode")
	if !changed || settings.ReasoningEffort != "" {
		t.Fatalf("dropped effort capability must clear the pick: %+v changed=%v", settings, changed)
	}
}
