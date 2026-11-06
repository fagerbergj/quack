"""Reads an .rmdoc zip on stdin and writes each notebook page to OUTDIR/<n>.svg with rmc; prints one JSON line.

{"fileType": "pdf"} for documents rmfakecloud exports itself, else {"fileType": ..., "pages": N} or {"error": {"page": N, "reason": ...}}.
"""
import io
import json
import sys
import zipfile
from pathlib import Path

EXPORTED_BY_RMFAKECLOUD = ("pdf", "epub")


def page_ids(content: dict) -> list[str]:
    # Current firmware lists pages under cPages (deleted ones flagged); older firmware under pages.
    c_pages = content.get("cPages", {}).get("pages")
    if c_pages is not None:
        return [p["id"] for p in c_pages if "deleted" not in p]
    return content.get("pages", [])


def main(out: Path) -> dict:
    z = zipfile.ZipFile(io.BytesIO(sys.stdin.buffer.read()))
    names = z.namelist()
    content = json.loads(z.read(next(n for n in names if n.endswith(".content"))))
    kind = content.get("fileType", "")
    if kind in EXPORTED_BY_RMFAKECLOUD:
        return {"fileType": kind}

    from rmc.exporters.svg import tree_to_svg  # imported late so PDF-type documents need no rmc
    from rmscene import read_tree

    files = {Path(n).stem: n for n in names if n.endswith(".rm")}
    # A page the tablet never drew on has no .rm file.
    ids = [i for i in page_ids(content) if i in files]
    for n, pid in enumerate(ids, 1):
        try:
            tree = read_tree(io.BytesIO(z.read(files[pid])))
            with open(out / f"{n}.svg", "w") as f:
                tree_to_svg(tree, f)
        except Exception as err:  # rmc raises many types on files it cannot read
            return {"error": {"page": n, "reason": f"{type(err).__name__}: {err}"[:300]}}
    return {"fileType": kind, "pages": len(ids)}


print(json.dumps(main(Path(sys.argv[1]))))
