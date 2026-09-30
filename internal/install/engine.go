package install

import "github.com/modullar/blade-runner/internal/core"

// ApplyEngine is the pipeline behind `apply`: gates first, then convergent steps, with
// progress recorded in the local state file.
func ApplyEngine(e *Env, rep core.Reporter) *core.Engine {
	return &core.Engine{Gates: ApplyGates(e), Steps: ApplySteps(e), Store: e.State, Rep: rep}
}

// RemoveEngine is the pipeline behind `remove`. It has no state store: its last step
// deletes the state file, and recording progress would recreate it.
func RemoveEngine(e *Env, rep core.Reporter) *core.Engine {
	return &core.Engine{Steps: RemoveSteps(e), Rep: rep}
}
