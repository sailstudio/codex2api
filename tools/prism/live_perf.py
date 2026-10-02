#!/usr/bin/env python3
"""Loopback-only live probes. Credentials are read privately; output is allowlisted."""
import argparse
import base64
import concurrent.futures
import http.client
import json
import math
from pathlib import Path
import threading
import time
import urllib.request

ROOT = Path(__file__).resolve().parents[2]
MODEL = "prism-gpt-5.6-sol"


def health(port=8081, path="/health/prism"):
    with urllib.request.urlopen(f"http://127.0.0.1:{port}{path}", timeout=5) as r:
        return json.load(r)


def safe_error(event):
    error = event.get("error") or {}
    msg = error.get("message", "") if isinstance(error, dict) else ""
    # Only gateway-generated structural codes; arbitrary upstream text is excluded.
    import re
    return "deadline_exceeded" if msg == "context deadline exceeded" else "admission_busy" if msg == "Prism admission capacity exhausted" else msg if re.fullmatch(r"Prism [a-z0-9_]+ \(HTTP [0-9]{3}\)", msg) else "redacted"


def call(body, path="/v1/chat/completions", barrier=None):
    key = (ROOT / ".secrets/api_key_8081").read_text().strip()
    if barrier:
        barrier.wait()
    start = time.monotonic()
    conn = http.client.HTTPConnection("127.0.0.1", 8081, timeout=330)
    result = {"http": None, "first_frame_s": None, "ttfp_s": None,
              "completed": False, "error": False, "chars": 0, "events": 0}
    text = ""
    payload = {}
    try:
        conn.request("POST", path, json.dumps(body),
                     {"Authorization": "Bearer " + key, "Content-Type": "application/json"})
        r = conn.getresponse()
        result["http"] = r.status
        if body.get("stream") and r.status == 200:
            terminal = False
            done = False
            while True:
                line = r.readline()
                if not line:
                    break
                if result["first_frame_s"] is None:
                    result["first_frame_s"] = time.monotonic() - start
                if not line.startswith(b"data: "):
                    continue
                raw = line[6:].strip()
                if raw == b"[DONE]":
                    done = True
                    continue
                event = json.loads(raw)
                result["events"] += 1
                if event.get("error") or event.get("type") in ("error", "response.failed"):
                    result["error"] = True
                    result["error_code"] = safe_error(event)
                delta = ""
                for choice in event.get("choices", []):
                    delta += choice.get("delta", {}).get("content") or ""
                    terminal |= choice.get("finish_reason") is not None
                if event.get("type") == "response.output_text.delta":
                    delta = event.get("delta", "")
                if event.get("type") == "response.completed":
                    terminal = done = True
                if delta and result["ttfp_s"] is None:
                    result["ttfp_s"] = time.monotonic() - start
                text += delta
            result["completed"] = terminal and done and not result["error"]
        else:
            payload = json.loads(r.read())
            text = payload.get("output_text", "")
            if "choices" in payload:
                text = payload["choices"][0].get("message", {}).get("content") or ""
            result["completed"] = r.status == 200 and not payload.get("error")
            result["error"] = not result["completed"]
            if result["error"]:
                result["error_code"] = safe_error(payload)
    except Exception as exc:
        # Never print exception messages, request headers, or upstream bodies.
        result["error"] = True
        result["exception_type"] = type(exc).__name__
    finally:
        conn.close()
    result["elapsed_s"] = round(time.monotonic() - start, 4)
    result["chars"] = len(text)
    for k in ("first_frame_s", "ttfp_s"):
        if result[k] is not None:
            result[k] = round(result[k], 4)
    result["error"] |= not result["completed"]
    return result, text, payload


def metrics():
    key = (ROOT / ".secrets/api_key_8081").read_text().strip()
    req = urllib.request.Request("http://127.0.0.1:8081/metrics/prism", headers={"Authorization": "Bearer " + key})
    with urllib.request.urlopen(req, timeout=5) as r:
        return {line.split()[0]: float(line.split()[1]) for line in r.read().decode().splitlines()
                if line and not line.startswith("#")}


def write(path, obj):
    with (ROOT / path).open("a") as f:
        f.write(json.dumps(obj, sort_keys=True) + "\n")
    print(json.dumps(obj, sort_keys=True), flush=True)


def e2e():
    dest = "logs/prism-live-e2e-evidence-v2.txt"
    write(dest, {"run_at_utc": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
                 "health_before": health(), "stock_health": health(8080, "/health")})
    body = {"model": MODEL, "store": False, "stream": True,
            "messages": [{"role": "user", "content": "Reply with exactly PONG."}]}
    result, text, _ = call(body)
    write(dest, {"test": "chat_stream", **result, "text_matches": text.strip() == "PONG"})
    prev = None
    for n, prompt in enumerate(["Remember the code MAPLE-4827. Reply exactly ACK.",
                                "What code did I tell you? Reply only the code.",
                                "Repeat the code again, exactly."], 1):
        body = {"model": MODEL, "store": True, "input": prompt}
        if prev:
            body["previous_response_id"] = prev
        result, text, payload = call(body, "/v1/responses")
        write(dest, {"test": f"responses_turn_{n}", **result,
                     "text": text[:160], "text_matches": (text.strip() == "ACK" if n == 1 else "MAPLE-4827" in text)})
        prev = payload.get("id")
        if not result["completed"] or not prev:
            break
    image = base64.b64encode((ROOT / "testdata/prism/vision-readable.png").read_bytes()).decode()
    result, text, _ = call({"model": MODEL, "store": False, "stream": True,
                           "messages": [{"role": "user", "content": [
                               {"type": "text", "text": "Read the text in this image and describe the two colored shapes. If you cannot see it, say Unavailable."},
                               {"type": "image_url", "image_url": {"url": "data:image/png;base64," + image}}]}]})
    write(dest, {"test": "vision_480x240_png", **result,
                 "text": text[:300], "ocr_matches": "ORBIT" in text and "739" in text,
                 "shapes_match": "red" in text.lower() and "blue" in text.lower()})
    write(dest, {"health_after": health(), "metrics": metrics(),
                 "stock_health": health(8080, "/health")})


def percentile(values, p):
    return sorted(values)[max(0, math.ceil(len(values) * p) - 1)] if values else None


def bench(label):
    dest = "logs/prism-perf-10conc.txt"
    ready_deadline = time.monotonic() + 100
    while True:
        before = health()
        if before["warm_slots"] >= before["capacity"] or time.monotonic() >= ready_deadline:
            break
        time.sleep(1)
    barrier = threading.Barrier(10)
    start = time.monotonic()
    def worker(i):
        result, _, _ = call({"model": MODEL, "store": False, "stream": True,
                             "stream_options": {"include_usage": True},
                             "messages": [{"role": "user", "content":
                                           f"Request {i}. Explain how rain forms in about 120 words. Respond directly in plain prose."}]},
                            barrier=barrier)
        return {"request": i, **result}
    samples = []
    with concurrent.futures.ThreadPoolExecutor(max_workers=10) as pool:
        futures = [pool.submit(worker, i) for i in range(10)]
        peak = 0
        peak_queued = 0
        peak_paced = 0
        while not all(f.done() for f in futures):
            snapshot = health()
            peak = max(peak, snapshot["inflight"])
            peak_queued = max(peak_queued, snapshot["queued"])
            peak_paced = max(peak_paced, snapshot.get("paced_waiters", 0))
            time.sleep(0.25)
        samples = [f.result() for f in futures]
    wall = time.monotonic() - start
    good = [s for s in samples if s["completed"]]
    ttfp = [s["ttfp_s"] for s in good if s["ttfp_s"] is not None]
    first = [s["first_frame_s"] for s in samples if s["first_frame_s"] is not None]
    chars = sum(s["chars"] for s in good)
    obj = {"label": label, "run_at_utc": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
           "concurrency": 10, "peak_inflight_observed": peak, "peak_queued_observed": peak_queued, "peak_paced_waiters_observed": peak_paced, "completed": len(good),
           "errors": 10 - len(good), "wall_s": round(wall, 4),
           "ttfp_p50_s": percentile(ttfp, .5), "ttfp_p95_s": percentile(ttfp, .95),
           "streams_started_before_first_completion": sum(s["first_frame_s"] is not None and s["first_frame_s"] < min(row["elapsed_s"] for row in samples) for s in samples),
           "first_frame_p50_s": percentile(first, .5), "first_frame_p95_s": percentile(first, .95),
           "aggregate_chars_per_s": round(chars / wall, 3),
           "rough_aggregate_tps": round(chars / 4 / wall, 3),
           "tps_method": "completed output characters / 4 / batch wall time; approximate, not billing",
           "health_before": before, "health_after": health(), "samples": samples}
    write(dest, obj)


if __name__ == "__main__":
    parser = argparse.ArgumentParser()
    parser.add_argument("mode", choices=("e2e", "bench"))
    parser.add_argument("--label", default="warm")
    args = parser.parse_args()
    e2e() if args.mode == "e2e" else bench(args.label)
