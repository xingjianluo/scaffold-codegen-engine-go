package engine

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/xingjianluo/scaffold-codegen-engine-go/plan"
	"github.com/xingjianluo/scaffold-codegen-engine-go/state"
)

// classify compares the desired content of one target against both the last
// committed manifest and the current file on disk. Content equality is
// decided by SHA-256. The returned digest is always the desired digest.
func (e *Engine) classify(t plan.Target, desired []byte) (state.Status, string, string, error) {
	desiredDigest := state.Digest(desired)
	abs := filepath.Join(e.root, filepath.FromSlash(t.OutputPath))

	owned, wasTracked := e.manifest.Targets[t.Name]

	raw, err := os.ReadFile(abs)
	if os.IsNotExist(err) {
		if wasTracked {
			// The engine wrote this path before; the user deleted it. We do
			// not silently re-create a deliberate deletion.
			return state.StatusUserModified,
				"engine-owned output was deleted externally; use --force to recreate it",
				desiredDigest, nil
		}
		return state.StatusMissing, "output does not exist yet", desiredDigest, nil
	}
	if err != nil {
		return "", "", desiredDigest,
			fmt.Errorf("read output %s: %w", t.OutputPath, err)
	}
	diskDigest := state.Digest(raw)

	if wasTracked {
		lastDigest := state.Digest([]byte(owned.Rendered))
		switch {
		case diskDigest != lastDigest:
			return state.StatusUserModified,
				"engine-owned file was changed externally (disk=" + diskDigest +
					", last-generated=" + lastDigest + ")", desiredDigest, nil
		case diskDigest == desiredDigest:
			return state.StatusUpToDate,
				"content already matches the current plan", desiredDigest, nil
		default:
			// File is untouched since the last run but the plan changed.
			return state.StatusPlanChanged,
				"file is unchanged from the previous run but the plan now renders different content",
				desiredDigest, nil
		}
	}
	if diskDigest == desiredDigest {
		return state.StatusForeign,
			"an identical untracked file already exists; use --force to adopt it",
			desiredDigest, nil
	}
	return state.StatusForeign,
		"an unrelated, untracked file exists at the target path; use --force to overwrite it",
		desiredDigest, nil
}
