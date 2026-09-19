package jev

// AdvisoryQuestions is the pipeline's fixed set of advisory questions. The
// advisory Jev step evaluates the run's diff against exactly these, and the
// measurement harness replays the same set over historical PRs, so both must
// come from here: changing this list changes what the signal means and
// invalidates any published tally against it.
//
// The set is deliberately small and each question targets a class the rest of
// the pipeline treats as serious: a contract break callers feel, a destructive
// data path, a security exposure, and a plain logic bug. These are advisory
// signals about the change as a whole, never gates.
//
// Names are stable identifiers (they key findings and tally rows); treat
// editing a name as a breaking change to the advisory vocabulary.
func AdvisoryQuestions() []Question {
	return []Question{
		{
			Name: "breaking",
			Instructions: "Does this change break an existing contract that callers outside this diff rely on? " +
				"Consider removed or renamed public APIs, CLI flags, config keys, file formats, and documented behavior whose meaning changes.",
			TrueCriteria:  "An existing exported, documented, or otherwise depended-on interface, flag, key, format, or behavior is removed, renamed, or has its semantics changed for existing callers.",
			FalseCriteria: "The change is additive or internal-only; every existing interface and documented behavior keeps working as before for existing callers.",
		},
		{
			Name: "data_loss",
			Instructions: "Can this change destroy, corrupt, or irreversibly lose data that a user or operator has not asked to discard, in the change's intended use? " +
				"Consider deletions, overwrites, migrations, and writes that replace or drop stored state.",
			TrueCriteria:  "An intended path deletes, overwrites, corrupts, or irreversibly drops data outside what the change's purpose requires, or without an adequate safeguard or escape hatch.",
			FalseCriteria: "No intended path in this change destroys or corrupts data, or every destructive path is exactly what the change exists to perform and is safeguarded.",
		},
		{
			Name: "security",
			Instructions: "Does this change introduce a security vulnerability? " +
				"Consider injection, authentication or authorization bypasses, secret exposure, unsafe deserialization, path traversal, and command execution on untrusted input.",
			TrueCriteria:  "The change adds a concretely reachable vulnerability: untrusted input reaches a dangerous sink, a control is removed or bypassable, or a secret is exposed.",
			FalseCriteria: "The change introduces no reachable vulnerability; input handling, authorization, and secret handling are sound for the paths it adds or touches.",
		},
		{
			Name: "bug",
			Instructions: "Does this diff contain a likely logic bug that will produce wrong behavior in the change's intended use? " +
				"Consider inverted conditions, wrong operators, off-by-one errors, unhandled failure paths, and state that is read or written inconsistently.",
			TrueCriteria:  "A concrete defect is visible in the diff and reachable through the change's intended usage, producing wrong results or failures.",
			FalseCriteria: "No identifiable defect in the changed logic; the code does what the change's purpose requires on the paths it touches.",
		},
	}
}
