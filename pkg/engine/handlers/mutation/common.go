package mutation

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/go-logr/logr"
	kyvernov1 "github.com/kyverno/kyverno/api/kyverno/v1"
	engineapi "github.com/kyverno/kyverno/pkg/engine/api"
	"github.com/kyverno/kyverno/pkg/engine/internal"
	"github.com/kyverno/kyverno/pkg/engine/mutate"
	"github.com/kyverno/kyverno/pkg/engine/mutate/patch"
	engineutils "github.com/kyverno/kyverno/pkg/engine/utils"
	"github.com/kyverno/kyverno/pkg/utils/api"
	"github.com/mattbaird/jsonpatch"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

type forEachMutator struct {
	logger        logr.Logger
	rule          kyvernov1.Rule
	policyContext engineapi.PolicyContext
	foreach       []kyvernov1.ForEachMutation
	resource      resourceInfo
	nesting       int
	contextLoader engineapi.EngineContextLoader
}

func (f *forEachMutator) mutateForEach(ctx context.Context) *mutate.Response {
	var applyCount int
	allPatches := make([][]byte, 0)

	for _, foreach := range f.foreach {
		elements, err := engineutils.EvaluateList(foreach.List, f.policyContext.JSONContext())
		if err != nil {
			msg := fmt.Sprintf("failed to evaluate list %s: %v", foreach.List, err)
			return mutate.NewErrorResponse(msg, err)
		}

		mutateResp := f.mutateElements(ctx, foreach, elements)
		if mutateResp.Status == engineapi.RuleStatusError {
			return mutate.NewErrorResponse("failed to mutate elements", err)
		}

		if mutateResp.Status != engineapi.RuleStatusSkip {
			applyCount++
			if len(mutateResp.Patches) > 0 {
				f.resource.unstructured = mutateResp.PatchedResource
				allPatches = append(allPatches, mutateResp.Patches...)
			}
			f.logger.Info("mutateResp.PatchedResource", "resource", mutateResp.PatchedResource)
			if err := f.policyContext.JSONContext().AddResource(mutateResp.PatchedResource.Object); err != nil {
				f.logger.Error(err, "failed to update resource in context")
			}
		}
	}

	msg := fmt.Sprintf("%d elements processed", applyCount)
	if applyCount == 0 {
		return mutate.NewResponse(engineapi.RuleStatusSkip, f.resource.unstructured, allPatches, msg)
	}

	return mutate.NewResponse(engineapi.RuleStatusPass, f.resource.unstructured, allPatches, msg)
}

func (f *forEachMutator) mutateElements(ctx context.Context, foreach kyvernov1.ForEachMutation, elements []interface{}) *mutate.Response {
	f.policyContext.JSONContext().Checkpoint()
	defer f.policyContext.JSONContext().Restore()

	patchedResource := f.resource
	var allPatches [][]byte
	if foreach.RawPatchStrategicMerge != nil {
		engineutils.InvertedElement(elements)
	}

	for index, element := range elements {
		if element == nil {
			continue
		}

		f.policyContext.JSONContext().Reset()
		policyContext := f.policyContext.Copy()

		falseVar := false
		if err := engineutils.AddElementToContext(policyContext, element, index, f.nesting, &falseVar); err != nil {
			return mutate.NewErrorResponse(fmt.Sprintf("failed to add element to mutate.foreach[%d].context", index), err)
		}

		if err := f.contextLoader(ctx, foreach.Context, policyContext.JSONContext()); err != nil {
			return mutate.NewErrorResponse(fmt.Sprintf("failed to load to mutate.foreach[%d].context", index), err)
		}

		preconditionsPassed, err := internal.CheckPreconditions(f.logger, policyContext.JSONContext(), foreach.AnyAllConditions)
		if err != nil {
			return mutate.NewErrorResponse(fmt.Sprintf("failed to evaluate mutate.foreach[%d].preconditions", index), err)
		}

		if !preconditionsPassed {
			f.logger.Info("mutate.foreach.preconditions not met", "elementIndex", index)
			continue
		}

		var mutateResp *mutate.Response
		if foreach.ForEachMutation != nil {
			nestedForEach, err := api.DeserializeJSONArray[kyvernov1.ForEachMutation](foreach.ForEachMutation)
			if err != nil {
				return mutate.NewErrorResponse("failed to deserialize foreach", err)
			}

			m := &forEachMutator{
				rule:          f.rule,
				policyContext: f.policyContext,
				resource:      patchedResource,
				logger:        f.logger,
				foreach:       nestedForEach,
				nesting:       f.nesting + 1,
				contextLoader: f.contextLoader,
			}

			mutateResp = m.mutateForEach(ctx)
		} else {
			mutateResp = mutate.ForEach(f.rule.Name, foreach, policyContext, patchedResource.unstructured, element, f.logger)
		}

		if mutateResp.Status == engineapi.RuleStatusFail || mutateResp.Status == engineapi.RuleStatusError {
			return mutateResp
		}

		if len(mutateResp.Patches) > 0 {
			patchedResource.unstructured = mutateResp.PatchedResource
			allPatches = append(allPatches, mutateResp.Patches...)
		}
	}
	var sortedPatches []jsonpatch.JsonPatchOperation
	for _, p := range allPatches {
		var jp jsonpatch.JsonPatchOperation
		if err := json.Unmarshal(p, &jp); err != nil {
			return mutate.NewErrorResponse("failed to convert patch", err)
		}
		sortedPatches = append(sortedPatches, jp)
	}
	sortedPatches = patch.FilterAndSortPatches(sortedPatches)
	var finalPatches [][]byte
	for _, p := range sortedPatches {
		if data, err := p.MarshalJSON(); err != nil {
			return mutate.NewErrorResponse("failed to marshal patch", err)
		} else {
			finalPatches = append(finalPatches, data)
		}
	}
	return mutate.NewResponse(engineapi.RuleStatusPass, patchedResource.unstructured, finalPatches, "")
}

func buildRuleResponse(rule *kyvernov1.Rule, mutateResp *mutate.Response, info resourceInfo) *engineapi.RuleResponse {
	message := mutateResp.Message
	if mutateResp.Status == engineapi.RuleStatusPass {
		message = buildSuccessMessage(mutateResp.PatchedResource)
	}
	resp := engineapi.NewRuleResponse(
		rule.Name,
		engineapi.Mutation,
		message,
		mutateResp.Status,
	)
	if mutateResp.Status == engineapi.RuleStatusPass {
		resp = resp.WithPatches(mutateResp.Patches...)
		if len(rule.Mutation.Targets) != 0 {
			resp = resp.WithPatchedTarget(&mutateResp.PatchedResource, info.parentResourceGVR, info.subresource)
		}
	}
	return resp
}

func buildSuccessMessage(r unstructured.Unstructured) string {
	if r.Object == nil {
		return "mutated resource"
	}
	if r.GetNamespace() == "" {
		return fmt.Sprintf("mutated %s/%s", r.GetKind(), r.GetName())
	}
	return fmt.Sprintf("mutated %s/%s in namespace %s", r.GetKind(), r.GetName(), r.GetNamespace())
}
