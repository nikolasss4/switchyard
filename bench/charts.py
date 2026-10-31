"""Render the README charts from benchmark JSON, in light and dark variants.
Usage: uv run --no-project --with matplotlib bench/charts.py bench/results/<run> docs/img"""

import json
import sys
from pathlib import Path

import matplotlib

matplotlib.use("Agg")
import matplotlib.pyplot as plt  # noqa: E402

THEMES = {
    "light": dict(surface="#fcfcfb", text="#0b0b0b", muted="#52514e", grid="#e4e3df",
                  s1="#2a78d6", s2="#eb6834", s3="#1baf7a", bad="#d03b3b", band="#f0efec"),
    "dark": dict(surface="#1a1a19", text="#f0efec", muted="#c3c2b7", grid="#383835",
                 s1="#3987e5", s2="#d95926", s3="#199e70", bad="#d03b3b", band="#262624"),
}


def style(ax, t):
    ax.set_facecolor(t["surface"])
    for side in ("top", "right"):
        ax.spines[side].set_visible(False)
    for side in ("left", "bottom"):
        ax.spines[side].set_color(t["grid"])
    ax.tick_params(colors=t["muted"], labelsize=9)
    ax.yaxis.grid(True, color=t["grid"], linewidth=0.8)
    ax.set_axisbelow(True)


def overhead(run, out, mode, t):
    rows = [r for r in json.loads((run / "overhead.json").read_text())["rows"] if r["surface"] == "openai"]
    rates = [r["target_rps"] for r in rows]
    fig, ax = plt.subplots(figsize=(7.2, 3.6), facecolor=t["surface"])
    style(ax, t)
    for key, label, color in (("p50", "p50", t["s1"]), ("p95", "p95", t["s2"]), ("p99", "p99", t["s3"])):
        ys = [r[f"added_{key}_ms"] for r in rows]
        ax.plot(rates, ys, color=color, linewidth=2, marker="o", markersize=5,
                markeredgecolor=t["surface"], markeredgewidth=1.5, label=label)
        ax.annotate(label, (rates[-1], ys[-1]), xytext=(6, 0), textcoords="offset points",
                    color=t["text"], fontsize=9, va="center")
    ax.axhline(0, color=t["muted"], linewidth=0.8)
    ax.set_xscale("log")
    ax.set_xticks(rates, [str(r) for r in rates])
    ax.minorticks_off()
    ax.set_xlabel("requests per second (constant rate)", color=t["muted"], fontsize=9)
    ax.set_ylabel("latency added by the gateway (ms)", color=t["muted"], fontsize=9)
    ax.set_title("Latency the gateway adds (4-vCPU runner, Redis in the path)", color=t["text"], fontsize=11, loc="left")
    ax.legend(frameon=False, labelcolor=t["text"], fontsize=9, loc="upper left")
    fig.tight_layout()
    fig.savefig(out / f"overhead-{mode}.svg", facecolor=t["surface"])
    plt.close(fig)


def chaos(run, out, mode, t):
    d = json.loads((run / "chaos.json").read_text())
    tl = d["timeline"]
    xs = [s["t"] for s in tl]
    a = [s["served_by_primary"] for s in tl]
    b = [s["served_by_secondary"] for s in tl]
    f = [s["failed"] for s in tl]
    direct = [100 * s["direct_ok"] / max(1, s["direct_sent"]) for s in tl]
    fig, (ax, ax2) = plt.subplots(2, 1, figsize=(7.6, 4.8), sharex=True, facecolor=t["surface"],
                                  gridspec_kw={"height_ratios": [3, 1.3], "hspace": 0.18})
    for x in (ax, ax2):
        style(x, t)
        for p in d["phases"][1:3]:
            x.axvspan(p["from_s"] - 0.5, p["to_s"] - 0.5, color=t["band"], zorder=0)
    ax.bar(xs, a, width=0.9, color=t["s1"], label="served by primary")
    ax.bar(xs, b, width=0.9, bottom=a, color=t["s2"], label="served by fallback")
    ax.bar(xs, f, width=0.9, bottom=[i + j for i, j in zip(a, b, strict=True)], color=t["bad"], label="failed")
    top = max(i + j + k for i, j, k in zip(a, b, f, strict=True))
    for p in d["phases"]:
        ax.text((p["from_s"] + p["to_s"]) / 2 - 0.5, top * 1.04, p["name"], ha="center", color=t["muted"], fontsize=8.5)
    for s in tl:  # breaker-open strip
        if s["primary_breaker"] == "open":
            ax.add_patch(plt.Rectangle((s["t"] - 0.5, -top * 0.08), 1, top * 0.04, color=t["muted"], linewidth=0))
    ax.text(-0.5, -top * 0.15, "bar under the axis: primary's circuit breaker open", color=t["muted"], fontsize=8)
    ax.set_ylim(-top * 0.18, top * 1.12)
    ax.set_ylabel("requests per second", color=t["muted"], fontsize=9)
    ax.set_title("Chaos: primary degrades to 50% errors, then goes down (200 req/s)", color=t["text"], fontsize=11, loc="left", pad=30)
    ax.legend(frameon=False, labelcolor=t["text"], fontsize=8.5, ncols=3, loc="lower left", bbox_to_anchor=(0, 1.0))
    ax2.plot(xs, direct, color=t["s1"], linewidth=2)
    ax2.set_ylim(-5, 105)
    ax2.set_yticks([0, 50, 100])
    ax2.set_ylabel("success %", color=t["muted"], fontsize=9)
    ax2.set_title("same load sent straight to the primary", color=t["muted"], fontsize=9, loc="left")
    ax2.set_xlabel("seconds", color=t["muted"], fontsize=9)
    fig.savefig(out / f"chaos-{mode}.svg", facecolor=t["surface"], bbox_inches="tight")
    plt.close(fig)


def main():
    run, out = Path(sys.argv[1]), Path(sys.argv[2])
    out.mkdir(parents=True, exist_ok=True)
    plt.rcParams["svg.fonttype"] = "none"
    plt.rcParams["font.family"] = ["Helvetica", "Arial", "DejaVu Sans"]
    for mode, t in THEMES.items():
        overhead(run, out, mode, t)
        chaos(run, out, mode, t)
    print("charts in", out)


main()
