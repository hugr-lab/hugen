package perm

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/hugr-lab/hugen/pkg/auth/template"
)

// Merge composes the Tier-1 (config) and Tier-2 (remote) rule
// lists into a single Permission for (object, field). Behaviour:
//
//   - Disabled is OR'd: either tier disables → final.Disabled.
//   - Hidden   is OR'd.
//   - Data is shallow-merged: remote first, then config wins on
//     scalar conflict (config is the floor — operator-pinned args
//     cannot be relaxed by a Hugr role rule).
//   - Filter is AND-merged: both non-empty → "(config) AND (remote)".
//   - FromConfig / FromRemote record which tier(s) contributed.
//
// Templates inside Data and Filter are substituted via
// pkg/auth/template; the supplied template.Context provides
// $auth.user_id / $auth.role / $session.* tokens.
func Merge(config, remote []Rule, tctx template.Context, object, field string) (Permission, error) {
	confP, err := mergeRules(config, tctx, object, field)
	if err != nil {
		return Permission{}, fmt.Errorf("perm: merge config: %w", err)
	}
	remP, err := mergeRules(remote, tctx, object, field)
	if err != nil {
		return Permission{}, fmt.Errorf("perm: merge remote: %w", err)
	}

	out := Permission{
		Disabled:   confP.Disabled || remP.Disabled,
		Hidden:     confP.Hidden || remP.Hidden,
		FromConfig: confP.FromConfig, // mergeRules sets this on any match
		FromRemote: remP.FromConfig,  // remote rules are tagged the same
	}

	// Filter: AND-concat both non-empty; otherwise pick the
	// non-empty side.
	switch {
	case confP.Filter != "" && remP.Filter != "":
		out.Filter = "(" + confP.Filter + ") AND (" + remP.Filter + ")"
	case confP.Filter != "":
		out.Filter = confP.Filter
	case remP.Filter != "":
		out.Filter = remP.Filter
	}

	// Data: remote first, then config-wins shallow merge. Both
	// inputs are already template-substituted by mergeRules.
	merged, err := mergeDataConfigWinsRaw(remP.Data, confP.Data)
	if err != nil {
		return Permission{}, fmt.Errorf("perm: merge data: %w", err)
	}
	out.Data = merged
	return out, nil
}

// mergeRules collapses a single rule list into one Permission for
// (object, field). Wildcard `*` rules contribute first, then
// exact-field rules layer on top (more specific wins inside the
// same tier). Templates inside Data / Filter are substituted via
// pkg/auth/template using tctx.
func mergeRules(rules []Rule, tctx template.Context, object, field string) (Permission, error) {
	out := Permission{}
	matched := false

	apply := func(r Rule) error {
		if r.Type != object {
			return nil
		}
		if r.Field != "*" && r.Field != field {
			return nil
		}
		if r.Disabled {
			out.Disabled = true
		}
		if r.Hidden {
			out.Hidden = true
		}
		if len(r.Data) > 0 {
			merged, err := mergeDataConfigWins(out.Data, r.Data, tctx)
			if err != nil {
				return err
			}
			out.Data = merged
		}
		if r.Filter != "" {
			f := template.ApplyString(r.Filter, tctx)
			if out.Filter == "" {
				out.Filter = f
			} else {
				out.Filter = "(" + out.Filter + ") AND (" + f + ")"
			}
		}
		matched = true
		return nil
	}
	// Wildcards first so exact-field rules can override on
	// scalar conflict.
	for _, r := range rules {
		if r.Field == "*" {
			if err := apply(r); err != nil {
				return Permission{}, err
			}
		}
	}
	for _, r := range rules {
		if r.Field != "*" {
			if err := apply(r); err != nil {
				return Permission{}, err
			}
		}
	}
	if matched {
		// Caller (Merge / LocalPermissions) decides whether this
		// counts as FromConfig or FromRemote — we just flag that
		// at least one rule contributed.
		out.FromConfig = true
	}
	return out, nil
}

// mergeDataConfigWinsRaw is the post-template-substitution shallow
// JSON object merge: `winner` wins on scalar conflict; arrays are
// replaced wholesale; nested objects are not recursed (shallow).
// Either input may be empty; nil-safe.
func mergeDataConfigWinsRaw(prev, winner json.RawMessage) (json.RawMessage, error) {
	if len(winner) == 0 {
		return prev, nil
	}
	if len(prev) == 0 {
		return winner, nil
	}
	var pm, wm map[string]any
	if err := json.Unmarshal(prev, &pm); err != nil {
		return nil, errors.New("perm: data merge: previous is not an object")
	}
	if err := json.Unmarshal(winner, &wm); err != nil {
		return nil, errors.New("perm: data merge: winner is not an object")
	}
	for k, v := range wm {
		pm[k] = v
	}
	out, err := json.Marshal(pm)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(out), nil
}
