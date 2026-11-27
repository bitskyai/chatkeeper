#!/usr/bin/env python3
"""Run a downloaded binary and check its loopback desktop wizard."""
import http.cookiejar
import os
from pathlib import Path
import re
import subprocess
import sys
import tempfile
import time
import urllib.request

binary = str(Path(sys.argv[1]).resolve())
with tempfile.TemporaryDirectory() as home:
    env = {**os.environ, "CHATKEEPER_HOME": home}
    with tempfile.TemporaryFile(mode="w+") as output:
        process = subprocess.Popen(
            [binary, "run", "--bind", "127.0.0.1:18787", "--no-browser"],
            env=env, stdout=output, stderr=output,
        )
        try:
            for _ in range(120):
                if process.poll() is not None:
                    raise RuntimeError("binary exited before wizard became reachable")
                output.seek(0)
                match = re.search(r"http://127\.0\.0\.1:18787/[^\s]*", output.read())
                if match:
                    opener = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(http.cookiejar.CookieJar()))
                    with opener.open(match[0], timeout=5) as response:
                        assert response.status == 200
                        assert b"ChatKeeper" in response.read()
                    print("Downloaded binary: loopback wizard reachable")
                    break
                time.sleep(0.5)
            else:
                raise RuntimeError("wizard launch URL timed out")
        finally:
            process.terminate()
            try:
                process.wait(timeout=10)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait()
