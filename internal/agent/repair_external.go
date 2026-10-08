package agent

import (
	"fmt"

	"github.com/kunchenguid/no-mistakes/internal/agentcfg"
	"github.com/kunchenguid/no-mistakes/internal/runenv"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type RepairFactory struct {
	Name                   types.AgentName   `json:"name"`
	Bin                    string            `json:"bin"`
	ExtraArgs              []string          `json:"extra_args,omitempty"`
	ACPRegistryOverrides   map[string]string `json:"acp_registry_overrides,omitempty"`
	DisableProjectSettings bool              `json:"disable_project_settings,omitempty"`
	Profile                agentcfg.Profile  `json:"profile,omitempty"`
	Environment            runenv.Overlay    `json:"-"`
}

type repairFactoryAgent struct {
	Agent
	factory RepairFactory
}

func WithRepairFactory(a Agent, factory RepairFactory) Agent {
	return &repairFactoryAgent{Agent: a, factory: factory}
}

// NeutralizesGateInstructions preserves the wrapped adapter's verified
// project-instruction suppression capability. Adding the repair factory is
// transport metadata and must not make an otherwise safe gate agent fail the
// daemon's pre-launch safety check.
func (a *repairFactoryAgent) NeutralizesGateInstructions() bool {
	return NeutralizesGateInstructions(a.Agent)
}

type RepairAgentDescriptor struct {
	Kind     string                  `json:"kind"`
	Factory  *RepairFactory          `json:"factory,omitempty"`
	Preamble string                  `json:"preamble,omitempty"`
	Children []RepairAgentDescriptor `json:"children,omitempty"`
}

func DescribeRepairAgent(a Agent, purpose string) (RepairAgentDescriptor, runenv.Overlay, error) {
	switch current := a.(type) {
	case *repairFactoryAgent:
		factory := current.factory
		environment := factory.Environment.Clone()
		factory.Environment = runenv.Overlay{}
		return RepairAgentDescriptor{Kind: "factory", Factory: &factory}, environment, nil
	case steeredAgent:
		child, environment, err := DescribeRepairAgent(current.Agent, purpose)
		return RepairAgentDescriptor{Kind: "steering", Preamble: current.preamble, Children: []RepairAgentDescriptor{child}}, environment, err
	case *fallbackAgent:
		descriptor := RepairAgentDescriptor{Kind: "fallback"}
		var environment runenv.Overlay
		for _, childAgent := range current.agents {
			child, childEnvironment, err := DescribeRepairAgent(childAgent, purpose)
			if err != nil {
				return RepairAgentDescriptor{}, runenv.Overlay{}, err
			}
			descriptor.Children = append(descriptor.Children, child)
			environment = mergeRepairEnvironment(environment, childEnvironment)
		}
		return descriptor, environment, nil
	case *reviewAgents:
		selected := current.primary
		if purpose == "review-fix" {
			selected = current.fixAgent()
		} else if purpose == "review" && current.reviewer != nil {
			selected = current.reviewer
		}
		return DescribeRepairAgent(selected, purpose)
	case *noopAgent:
		return RepairAgentDescriptor{Kind: "noop"}, runenv.Overlay{}, nil
	default:
		return RepairAgentDescriptor{}, runenv.Overlay{}, fmt.Errorf("agent %T has no independent repair factory", a)
	}
}

func BuildRepairAgent(descriptor RepairAgentDescriptor) (Agent, error) {
	switch descriptor.Kind {
	case "factory":
		if descriptor.Factory == nil {
			return nil, fmt.Errorf("repair factory is missing")
		}
		factory := descriptor.Factory
		return NewWithOptions(factory.Name, factory.Bin, factory.ExtraArgs, Options{
			ACPRegistryOverrides:   factory.ACPRegistryOverrides,
			DisableProjectSettings: factory.DisableProjectSettings,
			Profile:                factory.Profile,
		})
	case "steering":
		if len(descriptor.Children) != 1 {
			return nil, fmt.Errorf("repair steering descriptor is invalid")
		}
		child, err := BuildRepairAgent(descriptor.Children[0])
		if err != nil {
			return nil, err
		}
		return steeredAgent{Agent: child, preamble: descriptor.Preamble}, nil
	case "fallback":
		children := make([]Agent, 0, len(descriptor.Children))
		for _, childDescriptor := range descriptor.Children {
			child, err := BuildRepairAgent(childDescriptor)
			if err != nil {
				return nil, err
			}
			children = append(children, child)
		}
		return NewFallback(children), nil
	case "noop":
		return NewNoop(), nil
	default:
		return nil, fmt.Errorf("unknown repair agent descriptor %q", descriptor.Kind)
	}
}

func mergeRepairEnvironment(base, extra runenv.Overlay) runenv.Overlay {
	if base.Set == nil {
		base.Set = make(map[string]string)
	}
	for key, value := range extra.Set {
		base.Set[key] = value
	}
	base.Unset = append(base.Unset, extra.Unset...)
	return base
}
