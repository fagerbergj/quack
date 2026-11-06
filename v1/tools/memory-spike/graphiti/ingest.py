"""Graphiti arm: ingest the case prefix into embedded FalkorDB Lite, then search once per probe.

Usage: graphiti/.venv/bin/python graphiti/ingest.py [ingest|search|all]
Writes results/graphiti-recall.json in the shape run.ts reads.
"""
import os

# Read at import time by graphiti_core: one model call at a time, the embedder's real width, no telemetry.
os.environ.update(SEMAPHORE_LIMIT='1', EMBEDDING_DIM='2560', GRAPHITI_TELEMETRY_ENABLED='false')
for k in ('OPENAI_API_KEY', 'OPENAI_BASE_URL'):
    os.environ.pop(k, None)

import asyncio, json, re, sys, time, traceback, urllib.request
from datetime import datetime
from pathlib import Path

from openai import AsyncOpenAI
from graphiti_core import Graphiti
from graphiti_core.cross_encoder.openai_reranker_client import OpenAIRerankerClient
from graphiti_core.driver.falkordb_driver import FalkorDriver
from graphiti_core.embedder.openai import OpenAIEmbedder, OpenAIEmbedderConfig
from graphiti_core.llm_client.config import LLMConfig
from graphiti_core.llm_client.openai_generic_client import OpenAIGenericClient
from graphiti_core.nodes import EpisodeType
from redislite.async_falkordb_client import AsyncFalkorDB

HERE = Path(__file__).resolve().parent.parent
BASE = 'http://100.64.0.6:11436/v1'
LLM, EMBED = 'jaison/qwen3.8-27b', 'jaison/qwen3-embed'
CAP_TOKENS = 2000  # same cap as run.ts
DB = HERE / 'graphiti' / 'db' / 'case.db'
stats = {'llm_calls': 0, 'llm_seconds': 0.0, 'llm_errors': [], 'malformed_json': 0, 'embed_calls': 0, 'embed_seconds': 0.0}


def wait_for_slots(limit=2):
    while True:
        try:
            text = urllib.request.urlopen('http://127.0.0.1:9993/metrics', timeout=5).read().decode()
            running = float(re.search(r'^vllm:num_requests_running\{[^}]*\} (\S+)', text, re.M).group(1))
        except Exception:
            running = 0
        if running <= limit:
            return
        time.sleep(15)


def local_client():
    # The key is a placeholder the SDK requires; base_url is pinned to llama-swap, so nothing can fall through to OpenAI.
    client = AsyncOpenAI(api_key='local-no-key', base_url=BASE)
    create, embed = client.chat.completions.create, client.embeddings.create

    async def counted_create(**kw):
        t = time.time()
        try:
            # Thinking off, as the reader runs it.
            r = await create(**kw, extra_body={'chat_template_kwargs': {'enable_thinking': False}})
        except Exception as e:
            stats['llm_errors'].append(f'{type(e).__name__}: {str(e)[:300]}')
            raise
        finally:
            stats['llm_calls'] += 1
            stats['llm_seconds'] += time.time() - t
        try:
            json.loads(r.choices[0].message.content or '')
        except Exception:
            stats['malformed_json'] += 1
            msg = f'malformed JSON ({r.choices[0].finish_reason}, {r.usage.completion_tokens} tokens, {time.time() - t:.0f} s): {(r.choices[0].message.content or "")[:300]!r}'
            stats['llm_errors'].append(msg)
            print('MALFORMED', msg, flush=True)
        return r

    async def counted_embed(**kw):
        t = time.time()
        try:
            return await embed(**kw)
        finally:
            stats['embed_calls'] += 1
            stats['embed_seconds'] += time.time() - t

    client.chat.completions.create, client.embeddings.create = counted_create, counted_embed
    return client


async def graphiti():
    client = local_client()
    # Graphiti's own default temperature (1): at 0 a greedy loop runs to max_tokens and every retry repeats it.
    cfg = LLMConfig(api_key='local-no-key', base_url=BASE, model=LLM, small_model=LLM)
    DB.parent.mkdir(parents=True, exist_ok=True)
    driver = FalkorDriver(falkor_db=AsyncFalkorDB(str(DB)), database='quack')
    g = Graphiti(
        graph_driver=driver,
        llm_client=OpenAIGenericClient(config=cfg, client=client),
        embedder=OpenAIEmbedder(OpenAIEmbedderConfig(embedding_model=EMBED, embedding_dim=2560), client=client),
        cross_encoder=OpenAIRerankerClient(config=cfg, client=client),
        max_coroutines=1,
    )
    await g.build_indices_and_constraints()
    return g


def load_case():
    case = json.loads((HERE / 'probes.json').read_text())
    eps = [json.loads(l) for l in (HERE / 'data' / 'episodes-all.jsonl').read_text().splitlines()]
    prefix = [next(e for e in eps if e['key'].endswith(n)) for n in case['case']['prefix']]
    return case['probes'], prefix


async def ingest(g, prefix):
    log = []
    for e in prefix:
        wait_for_slots()
        t, calls = time.time(), stats['llm_calls']
        try:
            await g.add_episode(name=e['key'], episode_body=e['text'], source=EpisodeType.text,
                                source_description='quack review session: requests, sealed reviews, final answer',
                                reference_time=datetime.fromisoformat(e['created'].replace('Z', '+00:00')), group_id='fagerbergj-quack')
            err = None
        except Exception as ex:
            err = f'{type(ex).__name__}: {str(ex)[:500]}'
            traceback.print_exc()
        row = {'key': e['key'], 'chars': len(e['text']), 'seconds': round(time.time() - t, 1), 'llm_calls': stats['llm_calls'] - calls, 'error': err}
        log.append(row)
        print(json.dumps(row), flush=True)
        (HERE / 'results' / 'graphiti-ingest.json').write_text(json.dumps({'episodes': log, 'stats': stats}, indent=2))
    return log


def render(edge):
    when = f"valid {edge.valid_at:%Y-%m-%d %H:%M}" if edge.valid_at else 'valid ?'
    if edge.invalid_at:
        when += f", invalid {edge.invalid_at:%Y-%m-%d %H:%M}"
    return f'- {edge.fact} ({when})'


async def search(g, probes):
    sections = {}
    for p in probes:
        wait_for_slots()
        edges = await g.search(p['q'], group_ids=['fagerbergj-quack'], num_results=40)
        picked, used = [], 0
        for e in edges:
            line = render(e)
            if used + len(line) // 4 + 1 > CAP_TOKENS:
                continue
            picked.append(e)
            used += len(line) // 4 + 1
        # Same rule as the episodes arm: picked by rank, rendered oldest first.
        picked.sort(key=lambda e: e.valid_at.timestamp() if e.valid_at else 0)
        sections[p['id']] = '\n'.join(render(e) for e in picked)
    return sections


async def main(mode):
    probes, prefix = load_case()
    (HERE / 'results').mkdir(exist_ok=True)
    if mode in ('ingest', 'all') and DB.exists():
        DB.unlink()
    g = await graphiti()
    t = time.time()
    if mode in ('ingest', 'all'):
        await ingest(g, prefix)
    ingest_seconds = time.time() - t
    prior = json.loads((HERE / 'results' / 'graphiti-ingest.json').read_text())
    if mode in ('search', 'all'):
        sections = await search(g, probes)
        counts = await g.driver.execute_query('MATCH (n:Entity) WITH count(n) AS nodes MATCH ()-[r:RELATES_TO]->() RETURN nodes, count(r) AS edges')
        out = {'sections': sections, 'ingestSeconds': sum(r['seconds'] for r in prior['episodes']) if mode == 'search' else ingest_seconds,
               'rows': counts[0][0] if counts and counts[0] else None, 'ingest': {**prior, 'search_stats': stats}}
        (HERE / 'results' / 'graphiti-recall.json').write_text(json.dumps(out, indent=2, default=str))
    await g.close()


if __name__ == '__main__':
    asyncio.run(main(sys.argv[1] if len(sys.argv) > 1 else 'all'))
