#!/usr/bin/env python3
"""Bootstrap standard error of the MRR difference between fusion and the best single arm.

P7 Task 7's decision rule needs a spread, and the spread that matters is not
run-to-run: retrieval here is deterministic (embed.Fake and nomic-embed-text are
both fixed, the golden set is derived mechanically, and Fuse's tie-break is
total), so two identical runs give identical numbers and a run-to-run sigma is
exactly zero. What is uncertain is WHICH QUESTIONS the golden set happens to
contain, so the spread is a sampling spread: resample the questions with
replacement, recompute the difference on each resample, take the standard
deviation.

It needs no extra retrieval runs — only the per-question ranks P6 recorded.

Usage: python3 docs/eval/bootstrap.py [B] [seed]
"""

import glob
import json
import os
import random
import statistics
import sys

RUNS = os.path.join(os.path.dirname(os.path.abspath(__file__)), "runs")
MODES = ("vector", "hybrid", "lexical")


def load(mode):
    hits = glob.glob(os.path.join(RUNS, f"*-{mode}-*.json"))
    if len(hits) != 1:
        raise SystemExit(f"want exactly one {mode} run, found {len(hits)}: {hits}")
    with open(hits[0]) as f:
        return json.load(f), os.path.basename(hits[0])


def rr(case):
    """Reciprocal rank: 0 when the gold span was not retrieved at all."""
    return 1.0 / case["gold_rank"] if case["gold_rank"] >= 1 else 0.0


def main():
    b = int(sys.argv[1]) if len(sys.argv) > 1 else 1000
    seed = int(sys.argv[2]) if len(sys.argv) > 2 else 20260903

    runs = {}
    for m in MODES:
        runs[m], name = load(m)
        print(f"{m:8s} {name}")

    for arm_i, arm in enumerate(("ast", "window")):
        ids = [c["case_id"] for c in runs["vector"]["arms"][arm_i]["cases"]]
        per = {}
        for m in MODES:
            cases = runs[m]["arms"][arm_i]["cases"]
            if [c["case_id"] for c in cases] != ids:
                raise SystemExit(f"{arm}/{m}: the golden set differs; the comparison is not paired")
            per[m] = [rr(c) for c in cases]
            reported = runs[m]["arms"][arm_i]["summary"]["mrr"]
            got = sum(per[m]) / len(per[m])
            if abs(got - reported) > 1e-12:
                raise SystemExit(f"{arm}/{m}: recomputed MRR {got} != reported {reported}")

        n = len(ids)
        mrr = {m: sum(per[m]) / n for m in MODES}
        best_single = max(mrr["vector"], mrr["lexical"])
        diff = mrr["hybrid"] - best_single

        rng = random.Random(seed)
        diffs = []
        for _ in range(b):
            idx = [rng.randrange(n) for _ in range(n)]
            r = {m: sum(per[m][i] for i in idx) / n for m in MODES}
            diffs.append(r["hybrid"] - max(r["vector"], r["lexical"]))
        sigma = statistics.stdev(diffs)

        delta = 0.02
        threshold = max(2 * sigma, delta)
        print()
        print(f"arm={arm}  n={n}  B={b}  seed={seed}")
        for m in MODES:
            hit = {h["k"]: h["lenient"] for h in runs[m]["arms"][arm_i]["summary"]["hit_at"]}
            nf = runs[m]["arms"][arm_i]["summary"]["gold_not_found"]
            print(f"  MRR({m:7s}) = {mrr[m]:.4f}   hit@1={hit[1]:.4f} hit@5={hit[5]:.4f} "
                  f"hit@10={hit[10]:.4f}  gold_not_found={nf}")
        print(f"  MRR(H) - max(MRR(V), MRR(L)) = {diff:+.4f}")
        print(f"  sigma (bootstrap SE of that difference) = {sigma:.4f}")
        print(f"  threshold = max(2*sigma, delta) = max({2 * sigma:.4f}, {delta:.4f}) = {threshold:.4f}")
        if diff > threshold:
            branch = "2 (fusion helps)"
        elif abs(diff) <= threshold:
            branch = "3 (inconclusive)"
        else:
            branch = "4 (fusion loses)"
        winner = "vector" if mrr["vector"] >= mrr["lexical"] else "lexical"
        print(f"  BRANCH {branch}; best single arm = {winner}")


if __name__ == "__main__":
    main()
