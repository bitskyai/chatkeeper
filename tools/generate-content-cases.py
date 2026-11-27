"""Capture synthetic content cases by executing the prototype's own pytest suite.
Run with the prototype src/tests on PYTHONPATH and pytest installed.
"""
import copy
import json
import os
from pathlib import Path
import pytest
import threadkeep.common as common
import test_content_validation as cases

records = []
original = common.message_content

def record(message):
    case = {'name': os.environ.get('PYTEST_CURRENT_TEST', '').split('::')[-1],
            'message': copy.deepcopy(message)}
    try:
        files, texts = original(message)
        case.update(files=files, texts=texts, invalid=False)
        return files, texts
    except common.ArchiveError:
        case.update(invalid=True)
        raise
    finally:
        records.append(case)

common.message_content = record
cases.message_content = record
assert pytest.main([cases.__file__, '-q']) == 0
out = Path(__file__).resolve().parents[1] / 'internal/plugins/slack/testdata/content.json'
out.parent.mkdir(parents=True, exist_ok=True)
out.write_text(json.dumps(records, sort_keys=True, ensure_ascii=True, indent=2) + '\n')
