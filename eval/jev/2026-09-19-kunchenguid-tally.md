# Jev advisory signal - measured tally, 2026-09-19

First recorded run of the Jev advisory evaluation signal against real pull
request history, shipped with the step that produces it. This file is a dated
measurement artifact: it records what the signal did on one corpus at one
point in time, not a promise about future runs. Reproduce or extend it with
[`cmd/jev-tally`](/cmd/jev-tally) (same questions, same verdict rule as the
live pipeline step).

## Scope

- Corpus: the 60 most recent merged pull requests of
  `kunchenguid/no-mistakes` (the module's upstream repository), measured
  2026-09-19. 40 carried a no-mistakes attestation and were
  evaluated; 20 (release chores and PRs without an
  attestation) are excluded from scoring.
- Model: `typesafe-ai/jev` via Vercel AI Gateway, the step default.
- Questions: the fixed set the pipeline step asks (`breaking`, `bug`,
  `data_loss`, `security`), verdict threshold 0.6 (the default; the harness
  hardcodes it rather than reading operator config).

## Method

Ground truth is the pipeline's own recorded outcome, read from each PR's
no-mistakes attestation body: a PR is **positive** when the summary records
real findings or a fix round, **positive (auto-fixed)** when the pipeline's
fixes landed before merge, and **negative** when it is attested with none.
The evaluated input is the PR's merged diff, truncated exactly as the live
step truncates. Because an auto-fixed PR's merged diff is the *post-fix*
tree, a `clear` there reads the final state, not a miss, so auto-fixed PRs
are excluded from the recall denominator. `uncertain` verdicts count as
neither false positive nor false negative.

## Per-PR results

| PR | ground truth | verdict | probabilities | |
|---|---|---|---|---|
| #1115 fix(agent): disable compact-adviser in agent subprocesses | negative | clear | b=0.15 bug=0.12 dl=0.02 sec=0.06 | clean |
| #1114 fix(pipeline): park Test agent budget cuts for a decision instead of failing the run | negative | flagged | b=0.84 bug=0.32 dl=0.11 sec=0.13 | fp |
| #1100 fix(pipeline): report approved Test exceptions as passed-with-override | positive (auto-fixed) | flagged | b=0.60 bug=0.39 dl=0.11 sec=0.13 | pos-af |
| #1096 fix(agent): bound pipeline agent host filesystem searches | negative | clear | b=0.15 bug=0.14 dl=0.03 sec=0.06 | clean |
| #1095 fix(pipeline): keep review findings outstanding until verified | positive (auto-fixed) | flagged | b=0.65 bug=0.37 dl=0.16 sec=0.12 | pos-af |
| #1092 test(testgit): resolve real git independently of PATH | positive (auto-fixed) | clear | b=0.11 bug=0.32 dl=0.02 sec=0.06 | pos-af |
| #1081 feat(pipeline): integrate a moved base by merging behind rebase.strategy | negative | clear | b=0.16 bug=0.24 dl=0.11 sec=0.10 | clean |
| #1077 feat(config): add branch capture replacements | positive (auto-fixed) | clear | b=0.38 bug=0.30 dl=0.04 sec=0.11 | pos-af |
| #1072 feat(daemon): pin Pi model and reasoning effort per run | negative | uncertain | b=0.35 bug=0.42 dl=0.08 sec=0.16 | unc |
| #1070 fix(pipeline): attest approved test command failures | negative | flagged | b=0.63 bug=0.32 dl=0.13 sec=0.11 | fp |
| #1059 fix(agent): record honest token usage on failed and cancelled invocations | negative | flagged | b=0.83 bug=0.41 dl=0.06 sec=0.08 | fp |
| #1057 fix(pipeline): prevent fake TUI live-validation passes | negative | clear | b=0.06 bug=0.06 dl=0.02 sec=0.03 | clean |
| #1056 fix(shellenv): reliably resolve daemon login shell environment | positive (auto-fixed) | flagged | b=0.66 bug=0.31 dl=0.03 sec=0.09 | pos-af |
| #1051 fix(pipeline): rerun a fresh review when the reviewer's output fails schema validation | positive (auto-fixed) | clear | b=0.35 bug=0.31 dl=0.03 sec=0.11 | pos-af |
| #1050 fix(agent): tolerate provider residue and split objects in structured output | negative | clear | b=0.34 bug=0.34 dl=0.02 sec=0.07 | clean |
| #1048 docs: document PTY requirements for TUI validation | negative | clear | b=0.09 bug=0.07 dl=0.02 sec=0.03 | clean |
| #1046 fix(pipeline): handle empty-index repairs and reconcile stale private mirrors | positive (auto-fixed) | flagged | b=0.62 bug=0.37 dl=0.21 sec=0.17 | pos-af |
| #1044 feat(pipeline): support repository PR templates with author-preserving updates | negative | clear | b=0.32 bug=0.31 dl=0.16 sec=0.13 | clean |
| #1032 test(pipeline): use real git repo setup in gh-unavailable PR step test | negative | clear | b=0.03 bug=0.10 dl=0.03 sec=0.03 | clean |
| #1031 docs: add workflow-scope push failure troubleshooting | negative | clear | b=0.03 bug=0.05 dl=0.03 sec=0.05 | clean |
| #1030 feat(pipeline): add repository command gates | negative | clear | b=0.29 bug=0.24 dl=0.07 sec=0.15 | clean |
| #1024 fix: publish update channels after automated releases | positive (auto-fixed) | clear | b=0.17 bug=0.35 dl=0.06 sec=0.08 | pos-af |
| #1020 feat(config): add ticket-aware commit and PR titles | negative | uncertain | b=0.48 bug=0.52 dl=0.28 sec=0.20 | unc |
| #1018 feat(pipeline): add opt-out for generated PR intent publication | negative | clear | b=0.33 bug=0.26 dl=0.05 sec=0.08 | clean |
| #1016 feat(agent): configure independent reviewer and fixer harness profiles | negative | clear | b=0.14 bug=0.38 dl=0.04 sec=0.09 | clean |
| #1014 fix(pipeline): retry invalid test analyzer findings | positive (auto-fixed) | flagged | b=0.67 bug=0.48 dl=0.03 sec=0.16 | pos-af |
| #1012 feat(eval): auto-ingest fixed CI misses | positive (auto-fixed) | uncertain | b=0.48 bug=0.37 dl=0.10 sec=0.08 | pos-af |
| #1009 feat(pipeline): unify CI failures with findings loop | positive (auto-fixed) | flagged | b=0.78 bug=0.35 dl=0.12 sec=0.10 | pos-af |
| #1007 fix(update): fetch version metadata from release CDN | positive (auto-fixed) | flagged | b=0.80 bug=0.39 dl=0.10 sec=0.17 | pos-af |
| #1006 ci: split Windows git tests into parallel shards | positive (auto-fixed) | clear | b=0.14 bug=0.30 dl=0.02 sec=0.03 | pos-af |
| #1005 fix(pipeline): ask before proceeding without a live-testable surface | negative | uncertain | b=0.41 bug=0.37 dl=0.02 sec=0.11 | unc |
| #1003 fix(pipeline): distinguish intended local-main deliveries | positive (auto-fixed) | flagged | b=0.81 bug=0.38 dl=0.08 sec=0.07 | pos-af |
| #1001 fix(cli): show elapsed time for the active review round | positive (auto-fixed) | clear | b=0.35 bug=0.32 dl=0.07 sec=0.04 | pos-af |
| #999 feat(pipeline): add live validation to the test gate | positive (auto-fixed) | flagged | b=0.80 bug=0.19 dl=0.06 sec=0.07 | pos-af |
| #994 fix(pipeline): attest PR heads before pushing | positive (auto-fixed) | flagged | b=0.68 bug=0.39 dl=0.12 sec=0.14 | pos-af |
| #991 fix(eval): compare replay model names only | positive (auto-fixed) | flagged | b=0.80 bug=0.21 dl=0.04 sec=0.11 | pos-af |
| #989 fix(cli): stop telemetry from read-only commands | negative | flagged | b=0.88 bug=0.20 dl=0.10 sec=0.06 | fp |
| #988 fix(cli): keep AXI attached to slow daemons | positive (auto-fixed) | flagged | b=0.63 bug=0.46 dl=0.03 sec=0.06 | pos-af |
| #984 feat(cli): add ci-workflow subcommand to generate GitHub Actions CI from repo config | negative | clear | b=0.05 bug=0.34 dl=0.12 sec=0.14 | clean |
| #982 fix(eval): normalize provider-qualified model identities | negative | clear | b=0.33 bug=0.30 dl=0.04 sec=0.06 | clean |

Classes: `fp` false positive, `clean` clear-and-clean, `unc` uncertain
negative, `pos-af` auto-fixed positive (excluded from recall), `excl` no
attestation.

## Headline numbers

- **False positives (flagged, pipeline found nothing): 4** - #1114 #1070 #1059 #989 - every one a `breaking` flag in the 0.63-0.88 band on a deliberate behavior-changing fix whose own pipeline run recorded no findings.
- **False negatives (attested findings left unfixed at merge): 0**
- **Clear and clean: 14**; **uncertain: 3**
- **Auto-fixed positives (excluded): 19** - 12 flagged, 6 clear, 1 uncertain
- **Precision against the pipeline's own gates: 0.00** (12 of 19 auto-fixed PRs - the changes that genuinely needed repair - were flagged, but zero flags landed where the pipeline's review, test, and CI gates found nothing worth recording).

## Reading it

The signal reliably recognizes behavior and contract changes - it put high
`breaking` probabilities on 12/19 diffs that
needed pipeline repairs and stayed quiet on chores - but whether a behavior
change is a mistake or a deliberate decision is unknowable from a diff
alone, so on a mature repository where most behavior changes are deliberate
its flags concentrate exactly on the deliberate ones. Defect precision
against this repo's own gates is therefore 0.00, and that measurement - not
the raw detection rate - is why the step ships advisory-only: the verdict
and probabilities are reported next to the gates that own the decision,
and never gate a merge.

## Caveats

- The harness hardcodes the 0.6 threshold; an operator who tunes
  `jev.threshold` changes the live verdict rule, so a re-run tally and a
  live run can then disagree.
- The evaluation cache is keyed by PR and diff SHA only; re-running with a
  different `--model` reuses the recorded model's answers as cached.
- The ground-truth marker matches the attestation prose, so an unvalidated
  PR that merely mentions the pipeline in its body is counted as negative.
- All three biases are conservative (they understate rather than overstate
  the signal's precision).
