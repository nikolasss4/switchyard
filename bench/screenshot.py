"""Screenshot the provisioned Grafana dashboard (used by CI for the README).
Usage: uv run --no-project --with playwright bench/screenshot.py out.png"""

import sys
import time

from playwright.sync_api import sync_playwright

out = sys.argv[1] if len(sys.argv) > 1 else "grafana.png"
url = "http://localhost:3000/d/switchyard/switchyard?orgId=1&from=now-10m&to=now&kiosk&theme=dark"
with sync_playwright() as p:
    browser = p.chromium.launch()
    page = browser.new_page(viewport={"width": 1600, "height": 1500}, device_scale_factor=1)
    page.goto(url, wait_until="networkidle")
    time.sleep(8)  # let every panel query Prometheus
    page.screenshot(path=out, full_page=True)
    browser.close()
print("wrote", out)
