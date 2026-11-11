import io
import os
import time
import uuid

import pytest
import requests
from PIL import Image

BASE = os.getenv("BASE_URL", "http://127.0.0.1:8007")


def call(method, path, **kwargs):
    return requests.request(method, BASE + path, timeout=12, **kwargs)


def user_headers():
    user = {"username": "qa_" + uuid.uuid4().hex, "password": "demo-password"}
    assert call("POST", "/register", json=user).status_code == 201
    response = call("POST", "/login", json=user)
    assert response.status_code == 200
    return {"Authorization": "Bearer " + response.json()["token"]}


@pytest.fixture(scope="module")
def auth():
    return user_headers()


@pytest.fixture(scope="module")
def image_bytes():
    buffer = io.BytesIO()
    Image.new("RGB", (16, 16), (255, 255, 255)).save(buffer, "PNG")
    return buffer.getvalue()


def submit(auth, data, filter="Negative", key=None):
    headers = dict(auth, **{"Content-Type": "image/png"})
    if key is not None:
        headers["Idempotency-Key"] = key
    return call("POST", "/task", params={"filter": filter}, data=data, headers=headers)


def terminal(auth, location):
    until = time.monotonic() + 35
    while time.monotonic() < until:
        response = call("GET", location, headers=auth)
        assert response.status_code == 200
        data = response.json()
        assert data["status"] in ("queued", "processing", "succeeded", "failed")
        if data["status"] in ("succeeded", "failed"):
            return data
        time.sleep(0.2)
    pytest.fail(f"Job did not finish: {location}")


@pytest.mark.parametrize("filter, pixel", [("Negative", 0), ("Black & White", 255)])
def test_image_pipeline(auth, image_bytes, filter, pixel):
    response = submit(auth, image_bytes, filter)
    assert response.status_code == 202
    job_id = response.json()["task_id"]
    assert str(uuid.UUID(job_id)) == job_id
    assert response.headers["Location"] == f"/status/{job_id}"
    assert terminal(auth, response.headers["Location"])["status"] == "succeeded"
    result = call("GET", f"/result/{job_id}", headers=auth)
    assert result.status_code == 200
    assert result.headers["Content-Type"] == "image/jpeg"
    with Image.open(io.BytesIO(result.content)) as image:
        assert image.format == "JPEG"
        assert image.size == (16, 16)
        assert image.convert("RGB").getpixel((8, 8)) == (pixel, pixel, pixel)


def test_idempotency_and_ownership(auth, image_bytes):
    from concurrent.futures import ThreadPoolExecutor

    key = uuid.uuid4().hex
    with ThreadPoolExecutor(max_workers=4) as pool:
        responses = list(pool.map(lambda _: submit(auth, image_bytes, key=key), range(4)))
    assert all(response.status_code == 202 for response in responses)
    assert len({response.json()["task_id"] for response in responses}) == 1
    assert submit(auth, image_bytes, filter="Soft Blur", key=key).status_code == 409
    job_id = responses[0].json()["task_id"]
    other = user_headers()
    assert call("GET", f"/status/{job_id}", headers=other).status_code == 404
    assert call("GET", f"/result/{job_id}", headers=other).status_code == 404
    assert submit(other, image_bytes, key=key).json()["task_id"] != job_id


def test_invalid_requests(auth, image_bytes):
    assert submit({}, image_bytes).status_code == 401
    assert submit(auth, image_bytes, filter="unknown").status_code == 400
    assert submit(auth, b"not an image").status_code == 415
    assert submit(auth, b"x" * (10 * 1024 * 1024 + 1)).status_code == 413
    assert call("GET", "/status/malformed", headers=auth).status_code == 404


def test_invalid_png_is_permanent_failure(auth):
    response = submit(auth, b"\x89PNG\r\n\x1a\n" + b"broken")
    assert response.status_code == 202
    result = terminal(auth, response.headers["Location"])
    assert result["status"] == "failed"
    assert result["error_code"] == "invalid_image"
    assert call("GET", f'/result/{result["task_id"]}', headers=auth).status_code == 409
