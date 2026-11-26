package agent

import (
	"context"

	actool "github.com/Autumn-27/norma/tool"
)

// Dependencies belongs to one Server and is set before the agent starts running.
type Dependencies struct {
	PromptOverride                 func(string) (string, bool)
	WrapupOverride                 func(string) (string, bool)
	WrapupMaxTurnsOverride         func(string) (int, bool)
	WrapupTaskTimeoutOverride      func(string) (string, bool)
	WrapupTaskTimeoutTurnsOverride func(string) (int, bool)
	ToolAugment                    func(context.Context, string) ([]actool.CoreTool, DeferredInfo, func())
	ToolResolve                    func(context.Context, string, []actool.CoreTool) []actool.CoreTool
	FindingTrafficBindingEnabled   func() bool
}

// Legacy package hooks remain available for existing embedders and tests. An
// explicitly supplied dependency set never falls back to another Server's hooks.
func dependencySet(sets []*Dependencies) Dependencies {
	if len(sets) > 0 && sets[0] != nil {
		return *sets[0]
	}
	return Dependencies{
		PromptOverride:                 PromptOverride,
		WrapupOverride:                 WrapupOverride,
		WrapupMaxTurnsOverride:         WrapupMaxTurnsOverride,
		WrapupTaskTimeoutOverride:      WrapupTaskTimeoutOverride,
		WrapupTaskTimeoutTurnsOverride: WrapupTaskTimeoutTurnsOverride,
		ToolAugment:                    ToolAugment,
		ToolResolve:                    ToolResolve,
		FindingTrafficBindingEnabled:   FindingTrafficBindingEnabled,
	}
}

func (w *Worker) SetDependencies(dependencies *Dependencies)    { w.dependencies = dependencies }
func (p *Planner) SetDependencies(dependencies *Dependencies)   { p.dependencies = dependencies }
func (m *MainAgent) SetDependencies(dependencies *Dependencies) { m.dependencies = dependencies }
func (c *ChatAgent) SetDependencies(dependencies *Dependencies) { c.dependencies = dependencies }
func (t *ToolSet) SetDependencies(dependencies *Dependencies)   { t.dependencies = dependencies }

func (t *ToolSet) findingTrafficBindingEnabled() bool {
	return findingTrafficBindingEnabled(t.dependencies)
}

func firstDependency(sets []*Dependencies) *Dependencies {
	if len(sets) > 0 {
		return sets[0]
	}
	return nil
}
