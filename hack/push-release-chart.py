"""Retry Helm's manifest visibility race within one release job only."""
import pathlib
import re
import subprocess
import sys
import time


def push(chart, destination, log):
    original = chart.read_bytes()
    for attempt in range(3):
        if chart.read_bytes() != original:
            raise SystemExit('chart changed during publication')
        result = subprocess.run(
            ['helm', 'push', str(chart), destination],
            stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True, check=False,
        )
        print(result.stdout, end='', flush=True)
        log.write_text(result.stdout)
        if result.returncode == 0:
            return
        # GHCR can accept a manifest before Helm can read it back to tag it.
        # Retry only this failure, using the same archive in this job. A failed
        # workflow still consumes its version; the publication guard is unchanged.
        visibility_race = re.search(
            r'^Error: failed to perform "Tag" on destination: sha256:[a-f0-9]{64}: not found$',
            result.stdout, re.MULTILINE,
        )
        if not visibility_race or attempt == 2:
            raise SystemExit(result.returncode)
        delay = (5, 15)[attempt]
        print(f'Chart manifest not yet visible; retrying identical bytes in {delay}s', flush=True)
        time.sleep(delay)


if __name__ == '__main__':
    push(pathlib.Path(sys.argv[1]), sys.argv[2], pathlib.Path(sys.argv[3]))
