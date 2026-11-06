"""Regenerates the synthetic .rmdoc fixtures: python3 make-rmdoc.py (needs rmscene).

notebook.rmdoc is a one-page v6 notebook with a single handwritten-looking stroke; broken.rmdoc has a page rmscene cannot read; pdf.rmdoc is a PDF-type document that carries no pages of its own.
"""
import io
import json
import zipfile
from pathlib import Path
from uuid import UUID

from rmscene import CrdtId, CrdtSequenceItem, write_blocks
from rmscene import scene_items as si
from rmscene.scene_stream import (
    AuthorIdsBlock, MigrationInfoBlock, PageInfoBlock, SceneGroupItemBlock, SceneLineItemBlock, SceneTreeBlock, TreeNodeBlock,
)
from rmscene.tagged_block_common import LwwValue

DOC = "11111111-2222-3333-4444-555555555555"
PAGE = "66666666-7777-8888-9999-aaaaaaaaaaaa"


def item(item_id: CrdtId, value) -> CrdtSequenceItem:
    return CrdtSequenceItem(item_id=item_id, left_id=CrdtId(0, 0), right_id=CrdtId(0, 0), deleted_length=0, value=value)


def page() -> bytes:
    zigzag = [si.Point(x=-300 + i * 40.0, y=300 + (i % 2) * 400.0, speed=10, direction=0, width=24, pressure=200) for i in range(16)]
    line = si.Line(color=si.PenColor.BLACK, tool=si.Pen.FINELINER_2, points=zigzag, thickness_scale=2.0, starting_length=0.0)
    blocks = [
        AuthorIdsBlock(author_uuids={1: UUID(int=1)}),
        MigrationInfoBlock(migration_id=CrdtId(1, 1), is_device=True),
        PageInfoBlock(loads_count=1, merges_count=0, text_chars_count=1, text_lines_count=1),
        SceneTreeBlock(tree_id=CrdtId(0, 11), node_id=CrdtId(0, 0), is_update=True, parent_id=CrdtId(0, 1)),
        TreeNodeBlock(si.Group(node_id=CrdtId(0, 1))),
        TreeNodeBlock(si.Group(node_id=CrdtId(0, 11), label=LwwValue(timestamp=CrdtId(0, 12), value="Layer 1"))),
        SceneGroupItemBlock(parent_id=CrdtId(0, 1), item=item(CrdtId(0, 13), CrdtId(0, 11))),
        SceneLineItemBlock(parent_id=CrdtId(0, 11), item=item(CrdtId(1, 20), line)),
    ]
    out = io.BytesIO()
    write_blocks(out, blocks)
    return out.getvalue()


def rmdoc(path: Path, file_type: str, pages: dict[str, bytes]) -> None:
    content = {"fileType": file_type, "formatVersion": 2, "cPages": {"pages": [{"id": p, "idx": {"timestamp": "1:2", "value": "ba"}} for p in pages]}}
    with zipfile.ZipFile(path, "w", zipfile.ZIP_DEFLATED) as z:
        z.writestr(f"{DOC}.content", json.dumps(content))
        z.writestr(f"{DOC}.metadata", json.dumps({"type": "DocumentType", "visibleName": "Fixture"}))
        for p, data in pages.items():
            z.writestr(f"{DOC}/{p}.rm", data)


here = Path(__file__).parent
rmdoc(here / "notebook.rmdoc", "notebook", {PAGE: page()})
rmdoc(here / "broken.rmdoc", "notebook", {PAGE: b"not a v6 page"})
rmdoc(here / "pdf.rmdoc", "pdf", {})
