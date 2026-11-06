"""Mem0 arm: ingest the case prefix into Mem0 OSS on embedded Qdrant, then search once per probe.

Usage: mem0/.venv/bin/python mem0/ingest.py [ingest|search|all]
Writes results/mem0-recall.json in the shape run.ts reads.
"""
import os
from pathlib import Path

HERE = Path(__file__).resolve().parent.parent
# Read at import time by mem0 and fastembed: no telemetry, state under mem0/ (gitignored).
os.environ.update(MEM0_TELEMETRY='false', MEM0_DIR=str(HERE / 'mem0' / 'home'), FASTEMBED_CACHE_PATH=str(HERE / 'mem0' / 'cache'))
for k in ('OPENAI_API_KEY', 'OPENAI_BASE_URL', 'OPENAI_API_BASE', 'OPENROUTER_API_KEY'):
    os.environ.pop(k, None)

import json, re, shutil, sys, time, traceback, urllib.request

from mem0 import Memory

BASE = 'http://100.64.0.6:11436/v1'
LLM, EMBED = 'jaison/qwen3.8-27b', 'jaison/qwen3-embed'
CAP_TOKENS = 2000  # same cap as run.ts
SUBJECT = 'github:fagerbergj/quack'
# The embedder serves 4,096 tokens per request and Mem0 embeds each add's whole input, so an add stays under ~3,100.
ADD_CHARS = 12000
DB = HERE / 'mem0' / 'db'
stats = {'llm_calls': 0, 'llm_seconds': 0.0, 'llm_errors': [], 'malformed_json': 0, 'truncated': 0, 'embed_calls': 0, 'embed_seconds': 0.0, 'embed_errors': []}


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


def memory():
    cfg = {
        # The key is a placeholder the SDK requires; base_url is pinned to llama-swap, so nothing can fall through to OpenAI.
        'llm': {'provider': 'openai', 'config': {'model': LLM, 'openai_base_url': BASE, 'api_key': 'local-no-key'}},
        'embedder': {'provider': 'openai', 'config': {'model': EMBED, 'openai_base_url': BASE, 'api_key': 'local-no-key'}},
        'vector_store': {'provider': 'qdrant', 'config': {'collection_name': 'quack', 'embedding_model_dims': 2560, 'path': str(DB / 'qdrant'), 'on_disk': True}},
        'history_db_path': str(DB / 'history.db'),
    }
    m = Memory.from_config(cfg)
    create, embed = m.llm.client.chat.completions.create, m.embedding_model.client.embeddings.create

    def counted_create(**kw):
        wait_for_slots()
        t = time.time()
        try:
            # Thinking off, as the reader and the Graphiti arm run it; temperature, top_p and max_tokens stay Mem0's.
            r = create(**kw, extra_body={'chat_template_kwargs': {'enable_thinking': False}})
        except Exception as e:
            stats['llm_errors'].append(f'{type(e).__name__}: {str(e)[:300]}')
            raise
        finally:
            stats['llm_calls'] += 1
            stats['llm_seconds'] += time.time() - t
        c = r.choices[0]
        stats['truncated'] += c.finish_reason == 'length'
        try:
            json.loads(c.message.content or '')
        except Exception:
            stats['malformed_json'] += 1
            msg = f'malformed JSON ({c.finish_reason}, {r.usage.completion_tokens} tokens, {time.time() - t:.0f} s): {(c.message.content or "")[-300:]!r}'
            stats['llm_errors'].append(msg)
            print('MALFORMED', msg, flush=True)
        return r

    def counted_embed(**kw):
        t = time.time()
        try:
            return embed(**kw)
        except Exception as e:
            stats['embed_errors'].append(f'{type(e).__name__}: {str(e)[:300]}')
            raise
        finally:
            stats['embed_calls'] += 1
            stats['embed_seconds'] += time.time() - t

    m.llm.client.chat.completions.create, m.embedding_model.client.embeddings.create = counted_create, counted_embed
    return m


def load_case():
    case = json.loads((HERE / 'probes.json').read_text())
    eps = [json.loads(l) for l in (HERE / 'data' / 'episodes-all.jsonl').read_text().splitlines()]
    prefix = [next(e for e in eps if e['key'].endswith(n)) for n in case['case']['prefix']]
    return case['probes'], prefix


def parts(text):
    """Packs an episode's paragraphs, in order, into adds of at most ADD_CHARS."""
    out, buf = [], ''
    for p in text.split('\n\n'):
        if buf and len(buf) + len(p) + 2 > ADD_CHARS:
            out.append(buf)
            buf = ''
        buf += ('\n\n' if buf else '') + p
    return out + [buf]


def ingest(m, prefix):
    log = []
    for e in prefix:
        t, calls, added, errs = time.time(), stats['llm_calls'], 0, []
        for part in parts(e['text']):
            try:
                added += len(m.add(part, user_id=SUBJECT, metadata={'run': e['key'], 'run_created': e['created']})['results'])
            except Exception as ex:
                errs.append(f'{type(ex).__name__}: {str(ex)[:500]}')
                traceback.print_exc()
        row = {'key': e['key'], 'chars': len(e['text']), 'adds': len(parts(e['text'])), 'memories': added,
               'seconds': round(time.time() - t, 1), 'llm_calls': stats['llm_calls'] - calls, 'errors': errs}
        log.append(row)
        print(json.dumps(row), flush=True)
        (HERE / 'results' / 'mem0-ingest.json').write_text(json.dumps({'episodes': log, 'stats': stats}, indent=2))
    return log


def render(r):
    return f"- {r['memory']} ({r.get('metadata', {}).get('run_created', '?')[:16].replace('T', ' ')})"


def search(m, probes):
    sections = {}
    for p in probes:
        hits = m.search(p['q'], top_k=40, filters={'user_id': SUBJECT})['results']
        picked, used = [], 0
        for h in hits:
            n = len(render(h)) // 4 + 1
            if used + n > CAP_TOKENS:
                continue
            picked.append(h)
            used += n
        # Same rule as the other arms: picked by rank, rendered oldest first.
        picked.sort(key=lambda h: h.get('metadata', {}).get('run_created', ''))
        sections[p['id']] = '\n'.join(render(h) for h in picked)
    return sections


def main(mode):
    probes, prefix = load_case()
    (HERE / 'results').mkdir(exist_ok=True)
    if mode in ('ingest', 'all'):
        shutil.rmtree(DB, ignore_errors=True)
    DB.mkdir(parents=True, exist_ok=True)
    m = memory()
    t = time.time()
    if mode in ('ingest', 'all'):
        ingest(m, prefix)
    ingest_seconds = time.time() - t
    prior = json.loads((HERE / 'results' / 'mem0-ingest.json').read_text())
    if mode in ('search', 'all'):
        sections = search(m, probes)
        stored = m.get_all(filters={'user_id': SUBJECT}, top_k=10000)['results']
        (HERE / 'results' / 'mem0-memories.json').write_text(json.dumps(stored, indent=2, default=str))
        out = {'sections': sections, 'ingestSeconds': sum(r['seconds'] for r in prior['episodes']) if mode == 'search' else ingest_seconds,
               'rows': len(stored), 'ingest': {**prior, 'search_stats': stats}}
        (HERE / 'results' / 'mem0-recall.json').write_text(json.dumps(out, indent=2, default=str))
    m.close()


if __name__ == '__main__':
    main(sys.argv[1] if len(sys.argv) > 1 else 'all')
