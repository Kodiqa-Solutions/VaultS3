#!/usr/bin/env python3
"""Refresh the GitHub stars row of the comparison table in README.md.

Run weekly by .github/workflows/stars.yml. Reads each project's star count
from the GitHub API and rewrites the row and the date in its footnote. The
file is written only when a displayed figure changes, so a week with no
visible change makes no commit. Any failed lookup aborts without writing, so
the table never mixes fresh and stale figures.
"""
import datetime
import json
import os
import re
import sys
import urllib.request

README = os.path.join(os.path.dirname(__file__), "..", "..", "README.md")

# Column order of the table, with the footnote marker each cell carries.
COLUMNS = [
    ("Kodiqa-Solutions/VaultS3", ""),
    ("minio/minio", "¹"),
    ("pgsty/silo", ""),
    ("rustfs/rustfs", ""),
    ("seaweedfs/seaweedfs", ""),
    # Garage is developed on its own Forgejo instance, this is the official mirror.
    ("deuxfleurs-org/garage", ""),
]

ROW = re.compile(r"^\| GitHub stars³ \|.*\|$", re.M)
NOTE = re.compile(r"(> ³ Star counts read from each project's repository on )\d{1,2} [A-Z][a-z]+ \d{4}")


def stars(repo: str) -> int:
    req = urllib.request.Request(f"https://api.github.com/repos/{repo}")
    req.add_header("Accept", "application/vnd.github+json")
    req.add_header("User-Agent", "vaults3-readme-stars")
    token = os.environ.get("GITHUB_TOKEN")
    if token:
        req.add_header("Authorization", f"Bearer {token}")
    with urllib.request.urlopen(req, timeout=20) as resp:
        return int(json.load(resp)["stargazers_count"])


def fmt(n: int) -> str:
    """Same style as GitHub's own counter: 950, 1.7k, 36k."""
    if n < 1000:
        return str(n)
    if n < 10000:
        return f"{n / 1000:.1f}".rstrip("0").rstrip(".") + "k"
    return f"{round(n / 1000)}k"


def main() -> int:
    with open(README, encoding="utf-8") as f:
        text = f.read()
    if len(ROW.findall(text)) != 1 or len(NOTE.findall(text)) != 1:
        print("stars row or its footnote not found exactly once, table layout changed", file=sys.stderr)
        return 1

    cells = []
    for repo, mark in COLUMNS:
        n = stars(repo)
        cells.append(fmt(n) + mark)
        print(f"{repo}: {n} -> {fmt(n)}")
    row = "| GitHub stars³ | " + " | ".join(cells) + " |"

    if ROW.search(text).group(0) == row:
        print("no displayed figure changed")
        return 0

    today = datetime.date.today()
    date = f"{today.day} {today.strftime('%B %Y')}"
    text = ROW.sub(row, text)
    text = NOTE.sub(lambda m: m.group(1) + date, text)
    with open(README, "w", encoding="utf-8") as f:
        f.write(text)
    print("updated:", row)
    return 0


if __name__ == "__main__":
    sys.exit(main())
