package steps

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/testisolation"
)

func configureIsolatedWorkerContext(sctx *pipeline.StepContext, decoder *json.Decoder, emit func(isolatedStepEvent) error) {
	sctx.Log = func(text string) { emit(isolatedStepEvent{Kind: "log", Text: text}) }
	sctx.LogChunk = func(text string) { emit(isolatedStepEvent{Kind: "chunk", Text: text}) }
	sctx.LogFile = func(text string) { emit(isolatedStepEvent{Kind: "file", Text: text}) }
	sctx.RecordIsolatedHead = func(head string) error {
		pack, err := testisolation.ExportGitHead(sctx.Ctx, sctx.WorkDir, sctx.Run.HeadSHA, head)
		if err != nil {
			return err
		}
		if err := emit(isolatedStepEvent{Kind: "head", Text: head, Pack: pack}); err != nil {
			return err
		}
		var acknowledgement string
		if err := decoder.Decode(&acknowledgement); err != nil {
			return err
		}
		if acknowledgement != "" {
			return fmt.Errorf("%s", acknowledgement)
		}
		return nil
	}
}

func executeIsolatedWorkerStep(step pipeline.Step, sctx *pipeline.StepContext, emit func(isolatedStepEvent) error) error {
	outcome, stepErr := step.Execute(sctx)
	index, indexErr := testisolation.ExportGitIndex(sctx.Ctx, sctx.WorkDir, sctx.Run.HeadSHA)
	if err := errors.Join(stepErr, indexErr); err != nil {
		return emit(isolatedStepEvent{Kind: "error", Error: pipeline.EncodeIsolatedTestError(err), Index: index})
	}
	return emit(isolatedStepEvent{Kind: "result", Outcome: outcome, Index: index})
}
