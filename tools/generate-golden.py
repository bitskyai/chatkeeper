"""Run with prototype src and tests on PYTHONPATH; all content is synthetic.

Example: PYTHONPATH=../threadkeep/src:../threadkeep/tests python3 tools/generate-golden.py
The fixture helper and FolderStorage belong to the behavioural reference.
"""
from pathlib import Path
from test_viewer_port import snapshot, root, reply, user
from threadkeep.storage.folder import FolderStorage

out = Path(__file__).resolve().parents[1] / 'internal/storage/testdata/golden'
users = {'UOWNER': user('UOWNER', 'Fixture owner'),
         'UAUTHOR': user('UAUTHOR', 'Fixture author'),
         'UMENTION': user('UMENTION', 'Fixture mention')}
messages = [root('100.000001', 'Synthetic café 😀 <>& <@UMENTION>', replies=1,
                 reactions=[{'name': 'white_check_mark', 'users': ['UOWNER'], 'count': 1}],
                 blocks=[{'type': 'rich_text', 'block_id': 'synthetic', 'elements': [
                     {'type': 'rich_text_section', 'elements': [{'type': 'text', 'text': 'fixture'}]}]}],
                 files=[{'id': 'FFILE', 'name': 'fixture.bin', 'size': 5,
                         'url_private': 'https://files.slack.com/files-pri/T/FFILE'}]),
            reply('101.000001', '100.000001', 'Synthetic reply')]
payload = snapshot('100.000001', messages, users, {'FFILE': ('fixture.bin', b'\x00test')})
FolderStorage(out).save('CFIXTURE', '100.000001', payload)
(out / '.threadkeep.lock').unlink(missing_ok=True)
