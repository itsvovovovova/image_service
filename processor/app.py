import io
import os
import re
import warnings
from contextlib import closing
from datetime import datetime, timedelta, timezone

import boto3
from botocore.config import Config
from botocore.exceptions import ClientError
from flask import Flask, Response, jsonify, request
from PIL import Image, UnidentifiedImageError

from src.consumers.image_processor.filter_photo import filter_photo

app = Flask(__name__)
app.config["MAX_CONTENT_LENGTH"] = 10 * 1024 * 1024
Image.MAX_IMAGE_PIXELS = 20_000_000
KEY = re.compile(r"(?:source/[0-9a-f-]{36}|result/[0-9a-f-]{36}/[0-9a-f-]{36})")
bucket = os.environ.get("S3_BUCKET", "images")
s3 = boto3.client(
    "s3",
    endpoint_url=os.environ["S3_ENDPOINT"],
    config=Config(connect_timeout=2, read_timeout=5, retries={"total_max_attempts": 2},
                  s3={"addressing_style": "path"}),
)


@app.errorhandler(ClientError)
def s3_error(error):
    app.logger.warning("S3 operation failed: %s", error.response["Error"]["Code"])
    return "", 503


@app.get("/readyz")
def ready():
    s3.head_bucket(Bucket=bucket)
    return "", 204


@app.get("/objects")
def old_objects():
    params = {"Bucket": bucket, "MaxKeys": 500}
    if token := request.args.get("cursor"):
        params["ContinuationToken"] = token
    page = s3.list_objects_v2(**params)
    cutoff = datetime.now(timezone.utc) - timedelta(hours=24)
    keys = [
        obj["Key"] for obj in page.get("Contents", [])
        if KEY.fullmatch(obj["Key"]) and obj["LastModified"] < cutoff
    ]
    return jsonify(keys=keys, cursor=page.get("NextContinuationToken", ""))


@app.route("/objects/<path:key>", methods=["GET", "PUT", "DELETE"])
def objects(key):
    if not KEY.fullmatch(key):
        return "", 400
    if request.method == "PUT":
        if not key.startswith("source/"):
            return "", 400
        s3.put_object(Bucket=bucket, Key=key, Body=request.get_data())
        return "", 204
    if request.method == "DELETE":
        s3.delete_object(Bucket=bucket, Key=key)
        return "", 204
    obj = s3.get_object(Bucket=bucket, Key=key)

    def stream():
        with closing(obj["Body"]) as body:
            yield from body.iter_chunks(chunk_size=64 * 1024)

    response = Response(stream(), content_type="application/octet-stream")
    response.content_length = obj["ContentLength"]
    response.call_on_close(obj["Body"].close)
    return response


@app.post("/render")
def render():
    source = request.args.get("source", "")
    result = request.args.get("result", "")
    if (not KEY.fullmatch(source) or not source.startswith("source/")
            or not KEY.fullmatch(result) or not result.startswith("result/")):
        return "", 400
    obj = s3.get_object(Bucket=bucket, Key=source)
    with closing(obj["Body"]) as body:
        data = body.read(app.config["MAX_CONTENT_LENGTH"] + 1)
    if len(data) > app.config["MAX_CONTENT_LENGTH"]:
        return "", 422
    try:
        with warnings.catch_warnings():
            warnings.simplefilter("error", Image.DecompressionBombWarning)
            with Image.open(io.BytesIO(data), formats=["JPEG", "PNG", "GIF", "WEBP"]) as image:
                if getattr(image, "is_animated", False):
                    return "", 422
                image.load()
                normalized = io.BytesIO()
                image.convert("RGB").save(normalized, format="PNG")
            jpeg = filter_photo(normalized.getvalue(), request.args.get("filter", ""))
    except (UnidentifiedImageError, Image.DecompressionBombError, Image.DecompressionBombWarning,
            OSError, ValueError, SyntaxError):
        return "", 422
    s3.put_object(Bucket=bucket, Key=result, Body=jpeg, ContentType="image/jpeg")
    return "", 204

