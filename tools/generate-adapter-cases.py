"""Execute synthetic prototype adapter tests and record snapshot request traces.
Run with prototype src/tests on PYTHONPATH and pytest installed.
"""
import base64
import copy
import json
import os
from pathlib import Path
import pytest
from threadkeep.platforms.slack.web import SlackPlatform
import test_slack_web as cases

records = []
original = SlackPlatform.snapshot

def payload(value):
    if value is None:
        return None
    return {key: base64.b64encode(data if isinstance(data, bytes) else data.read_bytes()).decode()
            for key, data in value.items()}

def record(self, channel, root, previous=None):
    case = {'name': os.environ.get('PYTEST_CURRENT_TEST', '').split('::')[-1],
            'channel': channel, 'root': copy.deepcopy(root), 'team': self.team_id,
            'previous': payload(previous), 'trace': []}
    call, download = self.client.call, self.client.download
    def traced_call(method, **params):
        result = call(method, **params)
        case['trace'].append({'method': method, 'params': params, 'response': copy.deepcopy(result)})
        return result
    def traced_download(url):
        result = download(url)
        case['trace'].append({'method': 'download', 'url': url, 'bytes': base64.b64encode(result).decode()})
        return result
    self.client.call, self.client.download = traced_call, traced_download
    try:
        result = original(self, channel, root, previous=previous)
        case.update(invalid=False, expected=payload(result))
        return result
    except Exception:
        case.update(invalid=True)
        raise
    finally:
        self.client.call, self.client.download = call, download
        records.append(case)

SlackPlatform.snapshot = record
assert pytest.main([cases.__file__, '-q', '-k', 'not budget']) == 0
out = Path(__file__).resolve().parents[1] / 'internal/plugins/slack/testdata/adapter.json'
out.write_text(json.dumps(records, sort_keys=True, ensure_ascii=True, indent=2) + '\n')
