#!/usr/bin/env python3
"""A single-writer, monotonic Checks publisher, not a runner-owned job verdict.

The caller MUST serialize this entire action per PR with cancel-in-progress:
false. GitHub concurrency is not FIFO; the persisted workflow run_number and
run_attempt fence is what rejects late starts and reruns. Exact live body/head
comparison additionally covers same-head edits before any verdict was published.
There is no atomic PR-to-Checks CAS API: serialization protects check writers,
not PR edits. A subsequent edit schedules its own publisher; an old publisher
can never replace an already-published newer event, even if body/head recur.
"""
from __future__ import annotations

import contextlib
import importlib.util
import io
import json
import os
from pathlib import Path
import re
import sys
import urllib.request

_spec = importlib.util.spec_from_file_location(
    "nm_verify", Path(__file__).resolve().parent.parent / "require-no-mistakes" / "verify.py"
)
verify = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(verify)

CHECK_NAME = "PR must be raised via no-mistakes"
CHECK_SUMMARY_MAX = 65535
CHECK_SUMMARY_TRUNCATED = "\n\n[Event evidence truncated; full evidence remains in workflow logs.]"
PREFIX = "no-mistakes-current-v1:"
SHA = re.compile(r"[0-9a-f]{40}\Z")


class API:
    def __init__(self):
        self.repo = verify.env("GITHUB_REPOSITORY")
        self.token = verify.env("GITHUB_TOKEN")
        self.root = verify.env("GITHUB_API_URL") or "https://api.github.com"
        if not self.token or not re.fullmatch(r"[\w.-]+/[\w.-]+", self.repo):
            raise ValueError("publisher requires a token and repository identity")

    def request(self, method, path, data=None):
        request = urllib.request.Request(
            f"{self.root}/repos/{self.repo}/{path}",
            data=json.dumps(data).encode() if data is not None else None,
            method=method,
            headers={
                "Authorization": f"Bearer {self.token}",
                "Accept": "application/vnd.github+json",
                "Content-Type": "application/json",
                "X-GitHub-Api-Version": "2022-11-28",
            },
        )
        with urllib.request.urlopen(request, timeout=10) as response:
            return json.load(response)

    def checks(self, head):
        checks = []
        page = 1
        while True:
            payload = self.request("GET", f"commits/{head}/check-runs?filter=all&per_page=100&page={page}")
            batch = payload["check_runs"]
            if not isinstance(batch, list):
                raise ValueError("invalid check list")
            checks.extend(batch)
            if len(batch) < 100:
                return checks
            page += 1


def snapshot(pr):
    body = pr.get("body")
    head = pr.get("head", {}).get("sha")
    if body is None:
        body = ""
    if not isinstance(body, str) or not isinstance(head, str) or not SHA.fullmatch(head):
        raise ValueError("cannot establish exact PR body/head snapshot")
    return body, head


def evidence(pr):
    """Run the unchanged verifier against this event, never substituted live facts."""
    body, head = snapshot(pr)
    facts = object.__new__(verify.Facts)
    facts.body, facts.head_sha = body, head
    facts.author = pr.get("user", {}).get("login", "")
    facts.head_ref = pr.get("head", {}).get("ref", "")
    facts.number = str(pr["number"])
    log = io.StringIO()
    # check_* emit outputs on failure; keep those distinct from publisher outputs.
    old_output = os.environ.pop("GITHUB_OUTPUT", None)
    try:
        with contextlib.redirect_stdout(log), contextlib.redirect_stderr(log):
            reason = verify.exemption_reason(facts)
            if reason:
                return "exempt", reason
            try:
                verify.check_signature(facts)
                attestation = verify.parse_attestation(facts)
                verify.check_head_bind(facts, attestation["head_sha"])
                verify.check_required_steps(facts, attestation["steps"])
                verify.check_test_command_override(facts, attestation)
            except SystemExit as error:
                if error.code != 1:
                    raise
                return "failure", log.getvalue()
        return "success", "Structurally compliant, exactly head-bound pipeline attestation."
    finally:
        if old_output is not None:
            os.environ["GITHUB_OUTPUT"] = old_output


def check_summary(detail):
    if len(detail) <= CHECK_SUMMARY_MAX:
        return detail
    return detail[:CHECK_SUMMARY_MAX - len(CHECK_SUMMARY_TRUNCATED)] + CHECK_SUMMARY_TRUNCATED


def publish(api, pr, run, verdict, detail):
    """Called only inside the caller's non-cancelling, per-PR publisher lock."""
    number = pr["number"]
    rank = (run["run_number"], run["run_attempt"])
    workflow = run["workflow_id"]
    prefix = f"{PREFIX}{number}:"
    owned = []
    for check in api.checks(snapshot(pr)[1]):
        if check.get("name") != CHECK_NAME:
            continue
        external = check.get("external_id") or ""
        if not external.startswith(prefix):
            continue  # legacy job checks are retired separately at migration
        parts = external[len(prefix):].split(":")
        if len(parts) != 3 or any(not part.isdecimal() for part in parts):
            raise ValueError("malformed publisher watermark; refusing to overwrite")
        if int(parts[0]) != workflow or check.get("app", {}).get("slug") != "github-actions":
            raise ValueError("conflicting publisher identity; refusing to overwrite")
        owned.append((check, (int(parts[1]), int(parts[2]))))
    if len(owned) > 1:
        raise ValueError("ambiguous current verdict; refusing to overwrite")
    current = owned[0] if owned else None
    if current and current[1] > rank:
        return "obsolete"
    if current and current[1] == rank:
        return "unchanged"  # a retried POST/PATCH already committed this event
    # Last read before publication, under the same lock as the watermark read.
    live = api.request("GET", f"pulls/{number}")
    if live.get("number") != number:
        raise ValueError("live PR identity mismatch")
    if snapshot(live) != snapshot(pr):
        return "obsolete"
    data = {
        "name": CHECK_NAME,
        "status": "completed",
        "conclusion": "failure" if verdict == "failure" else "success",
        "external_id": f"{prefix}{workflow}:{rank[0]}:{rank[1]}",
        "details_url": run["html_url"],
        "output": {
            "title": f"Event {rank[0]}, attempt {rank[1]}: {verdict}",
            "summary": check_summary(detail),
        },
    }
    if current:
        api.request("PATCH", f"check-runs/{current[0]['id']}", data)
    else:
        data["head_sha"] = snapshot(pr)[1]
        api.request("POST", "check-runs", data)
    return "published"


def main():
    if verify.env("GITHUB_EVENT_NAME") != "pull_request_target":
        raise ValueError("publisher requires a trusted pull_request_target caller")
    with open(os.environ["GITHUB_EVENT_PATH"], encoding="utf-8") as handle:
        event = json.load(handle)
    pr = event["pull_request"]
    number = pr["number"]
    if type(number) is not int or number < 1 or event.get("number") != number:
        raise ValueError("invalid PR identity")
    if pr["base"]["repo"]["full_name"] != verify.env("GITHUB_REPOSITORY"):
        raise ValueError("event targets a different repository")
    verdict, detail = evidence(pr)
    verify.emit_output("event-verdict", verdict)
    # This is evidence, not a job failure or a fabricated successful verdict.
    # Prefix every line so an archived ::error:: cannot become a runner command.
    print(f"Original event verdict: {verdict}")
    for line in detail.splitlines():
        print(f"Evidence: {line}")
    summary = os.environ.get("GITHUB_STEP_SUMMARY")
    if summary:
        with open(summary, "a", encoding="utf-8") as handle:
            handle.write(f"Original event verdict: **{verdict}**\n\n")
    api = API()
    run = api.request("GET", f"actions/runs/{int(verify.env('GITHUB_RUN_ID'))}")
    if (run["event"] != "pull_request_target"
            or str(run["run_number"]) != verify.env("GITHUB_RUN_NUMBER")
            or str(run["run_attempt"]) != verify.env("GITHUB_RUN_ATTEMPT")
            or any(type(run[key]) is not int or run[key] < 1
                   for key in ("workflow_id", "run_number", "run_attempt"))):
        raise ValueError("cannot establish forge-owned event identity")
    result = publish(api, pr, run, verdict, detail)
    verify.emit_output("publication", result)
    print(f"Current verdict publication: {result}")
    if summary:
        with open(summary, "a", encoding="utf-8") as handle:
            handle.write(f"Current verdict publication: **{result}**\n")
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except Exception:
        # Do not expose URLs, response bodies, or credentials from API exceptions.
        sys.stderr.write("::error::Cannot safely publish current no-mistakes verdict; see caller permissions and serialization contract.\n")
        sys.exit(1)
