// Package model declares the runtime-side Model interface and the
// 5-step ModelRouter that the runtime uses to pick a provider for
// a given Hint.
//
// Resolution order (deterministic):
//
//  1. Hint.ModelOverride — explicit per-call override.
//  2. Hint.SessionModels[intent] — per-session intent map.
//  3. Hint.SkillModels[intent] — active-skill intent map.
//  4. runtime.Defaults[intent] — runtime default for the intent.
//  5. runtime.Defaults[IntentDefault] — terminal fallback.
//
// If all five steps fail, Resolve returns ErrModelUnavailable.
//
// See specs/001-agent-runtime-phase-1/contracts/model-router.md for
// the full contract.
package model
