#!/usr/bin/env python3
"""Regenerate the shared unicode61 corpus expectations with the Python reference.

Run with the reference checkout's Python environment and --reference CHECKOUT.
No live platform calls, credentials, or production archives are used.
"""
import argparse
import json
from pathlib import Path
import sys
import tempfile
from types import SimpleNamespace

parser = argparse.ArgumentParser()
parser.add_argument('--reference', required=True, type=Path)
args = parser.parse_args()
sys.path[:0] = [str(args.reference / 'src'), str(args.reference / 'tests')]
from threadkeep.search import SearchService  # noqa: E402
from threadkeep.storage.folder import FolderStorage  # noqa: E402
from test_viewer_port import USERS, snapshot  # noqa: E402

folder = Path(__file__).resolve().parents[1] / 'internal/search/testdata'
corpus = json.loads((folder / 'corpus.json').read_text())
with tempfile.TemporaryDirectory() as temp:
    names = sorted({t['workspace'] for t in corpus['threads']})
    workspaces = [SimpleNamespace(id=n, storage=SimpleNamespace(path=Path(temp) / n)) for n in names]
    stores = {ws.id: FolderStorage(ws.storage.path) for ws in workspaces}
    for thread in corpus['threads']:
        stores[thread['workspace']].save(thread['channel'], thread['root_ts'], snapshot(
            thread['root_ts'], thread['messages'], USERS, channel=thread['channel']))
    service = SearchService(workspaces)
    expected = []
    for query in corpus['queries']:
        options = dict(query)
        if 'from' in options:
            options['from_'] = options.pop('from')
        page = service.search(**options)
        expected.append([{k: hit[k] for k in ('workspace', 'channel', 'root_ts', 'score', 'snippet')}
                         for hit in page['results']])
    (folder / 'python-results.json').write_text(json.dumps(expected, ensure_ascii=False, indent=2) + '\n')
