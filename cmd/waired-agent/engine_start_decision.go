package main

// What an adopt trigger should bring up.
const (
	// engineStartOllama / engineStartVLLM name the engine to start. The
	// values are catalog.RuntimeOllama / catalog.RuntimeVLLM, because the
	// answer IS an engine kind and is compared against servingEngine().
	engineStartOllama = "ollama"
	engineStartVLLM   = "vllm"
	// engineStartNone: nothing is viable on this host yet. Not a failure —
	// the caller returns errEngineNotInstalled and leaves its latch open so
	// a later trigger retries.
	engineStartNone = "none"
)

// decideEngineStart answers which engine an adopt trigger should bring up.
//
//	current      servingEngine(): the decision this process booted with.
//	reChosen     what the boot rule (chooseEngine) answers against the LIVE
//	             host right now; "" when it finds nothing viable.
//	reChoiceOK   false when the live rule could not answer at all.
//	liveEngineUp whether the CURRENT engine has a process ready or starting.
//
// The rule this encodes is "do what a restart would do, without the
// restart". Before #339 the engine kind was frozen at construction, and
// engineViable(vllm) requires the venv to already exist — so a host that
// booted without one was pinned to ollama for the life of the process, and
// the venv the setup executor installed minutes later could not be taken on
// by any trigger. That is the ollama-binary problem #304 fixed, one level
// up: #304 replaced a boot-time binary snapshot with a live resolver, and
// this replaces the boot-time engine snapshot with a live re-choice.
//
// Two guards keep the re-choice from disturbing a host that is working:
//
//   - liveEngineUp wins over everything. An engine that is serving (or
//     mid-startup, which for vLLM is minutes on a multi-GB model) is never
//     swapped out from under itself; whatever the live rule now prefers can
//     wait for the next restart, which is where a switch has always
//     belonged.
//   - chooseEngine itself supplies the rest, at no cost here: a host with a
//     viable persisted state.Active is answered "persisted", so it reports
//     the engine it already has and no switch happens. A switch is only
//     reachable where a restart would have switched too.
//
// The answer is deliberately symmetric — it can name ollama for a process
// that booted on vLLM. That is the same rule read in the other direction,
// not a special case, and the guards above apply identically.
//
// Untagged so the table test runs on the darwin and windows legs as well,
// even though bootstrapVLLM is Linux-only (CLAUDE.md §Test discipline: put
// the seam below the behaviour under test).
func decideEngineStart(current, reChosen string, reChoiceOK, liveEngineUp bool) string {
	if liveEngineUp {
		return current
	}
	if !reChoiceOK {
		// No live answer, so there is nothing better than the boot
		// decision. Returning current rather than none matters: the ollama
		// arm has its own ollamaUsable() check and must still be reached.
		return current
	}
	if reChosen == "" {
		return engineStartNone
	}
	return reChosen
}
