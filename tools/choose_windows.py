#!/usr/bin/env python3
"""Choose the labeling windows (plan §10.1) and write config/labeling_windows.yaml.

24 windows of 15 minutes: each UTC hour of the day appears exactly once, the windows
are spread evenly over the backfill days (8 per day for 3 days), and each window
starts at a random minute inside its hour. The seed is recorded so the choice can be
reproduced.

    python3 tools/choose_windows.py --start "2026-09-25T07:06:17Z" --days 3 --seed 20260928
"""

import argparse
import datetime as dt
import random

WINDOW = dt.timedelta(minutes=15)


def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("--start", required=True, help="backfill start, UTC (RFC 3339)")
    ap.add_argument("--days", type=int, default=3)
    ap.add_argument("--seed", type=int, default=20260928)
    ap.add_argument("--out", default="config/labeling_windows.yaml")
    args = ap.parse_args()

    start = dt.datetime.fromisoformat(args.start.replace("Z", "+00:00"))
    rng = random.Random(args.seed)
    hours = list(range(24))
    rng.shuffle(hours)
    per_day = 24 // args.days

    windows = []
    for day in range(args.days):
        span_lo = start + dt.timedelta(days=day)
        span_hi = span_lo + dt.timedelta(days=1)
        for h in hours[day * per_day:(day + 1) * per_day]:
            # The one occurrence of hour h that starts inside this day's span (the
            # span starts mid-hour, so that hour's slot begins at the span start).
            t = span_lo.replace(minute=0, second=0, microsecond=0)
            while t.hour != h:
                t += dt.timedelta(hours=1)
            lo = max(t, span_lo)
            hi = min(t + dt.timedelta(hours=1), span_hi)
            latest = hi - WINDOW
            if latest < lo:
                raise SystemExit(f"no room for a window at hour {h} on day {day}")
            offset = rng.randint(0, int((latest - lo).total_seconds() // 60))
            ws = (lo + dt.timedelta(minutes=offset)).replace(second=0, microsecond=0)
            if ws < lo:
                ws += dt.timedelta(minutes=1)
            windows.append((ws, ws + WINDOW))
    windows.sort()

    fmt = lambda t: t.strftime("%Y-%m-%dT%H:%M:%SZ")
    with open(args.out, "w") as f:
        f.write("# Labeling windows (plan §10.1): 24 x 15 minutes, each UTC hour of the day once,\n")
        f.write(f"# {per_day} per day over {args.days} days from {fmt(start)}. Written by tools/choose_windows.py.\n")
        f.write(f"seed: {args.seed}\n")
        f.write(f"backfill_start: {fmt(start)}\n")
        f.write("windows:\n")
        for i, (a, b) in enumerate(windows):
            f.write(f"  - id: w{i:02d}\n    start: \"{fmt(a)}\"\n    end: \"{fmt(b)}\"\n")
    print(f"wrote {args.out}: {len(windows)} windows, hours covered: {len({a.hour for a, _ in windows})}")


if __name__ == "__main__":
    main()
