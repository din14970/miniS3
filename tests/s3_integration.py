#!/usr/bin/env python3
"""Integration tests for miniS3 using boto3 against a running server.

Usage: python3 tests/s3_integration.py --endpoint http://127.0.0.1:PORT [--access-key K --secret-key S]
Exits non-zero on the first failure; prints PASS lines per check.
"""
import argparse
import os
import sys
import tempfile
import urllib.request
import urllib.error

import boto3
import botocore
from botocore.config import Config
from botocore.exceptions import ClientError

PASS = 0


def ok(name):
    global PASS
    PASS += 1
    print(f"PASS: {name}")


def check(cond, name, extra=""):
    if not cond:
        print(f"FAIL: {name} {extra}")
        sys.exit(1)
    ok(name)


def make_client(endpoint, access_key="testkey", secret_key="testsecret"):
    return boto3.client(
        "s3",
        endpoint_url=endpoint,
        aws_access_key_id=access_key,
        aws_secret_access_key=secret_key,
        region_name="us-east-1",
        config=Config(s3={"addressing_style": "path"}, retries={"max_attempts": 2}),
    )


def expect_error(fn, code, name):
    try:
        fn()
    except ClientError as e:
        check(e.response["Error"]["Code"] == code, name, f"got {e.response['Error']['Code']}")
        return
    print(f"FAIL: {name} (no error raised)")
    sys.exit(1)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--endpoint", required=True)
    ap.add_argument("--bucket", default="testbucket")
    ap.add_argument("--access-key", default="testkey")
    ap.add_argument("--secret-key", default="testsecret")
    args = ap.parse_args()

    s3 = make_client(args.endpoint, args.access_key, args.secret_key)
    b = args.bucket

    # --- buckets ---
    names = [x["Name"] for x in s3.list_buckets()["Buckets"]]
    check(b in names, "list_buckets contains bucket", str(names))
    check(s3.head_bucket(Bucket=b)["ResponseMetadata"]["HTTPStatusCode"] == 200, "head_bucket")
    expect_error(lambda: s3.head_bucket(Bucket="nope-" + b), "404", "head_bucket missing -> 404")
    loc = s3.get_bucket_location(Bucket=b)
    check(loc["LocationConstraint"] in ("us-east-1", None, ""), "get_bucket_location", str(loc))
    s3.create_bucket(Bucket=b)  # idempotent
    ok("create_bucket idempotent")

    # --- basic object ops ---
    body = b"hello miniS3 " * 1000
    r = s3.put_object(Bucket=b, Key="docs/readme.txt", Body=body, ContentType="text/plain")
    check(r["ETag"].strip('"') == __import__("hashlib").md5(body).hexdigest(), "put_object etag is md5")
    g = s3.get_object(Bucket=b, Key="docs/readme.txt")
    check(g["Body"].read() == body, "get_object body matches")
    check(g["ContentType"] == "text/plain", "get_object content type")
    check(g["ETag"] == r["ETag"], "get_object etag matches put")
    check("LastModified" in g, "get_object last modified")
    h = s3.head_object(Bucket=b, Key="docs/readme.txt")
    check(h["ContentLength"] == len(body), "head_object size")
    expect_error(lambda: s3.get_object(Bucket=b, Key="docs/missing.txt"), "NoSuchKey", "get missing -> NoSuchKey")
    expect_error(lambda: s3.head_object(Bucket=b, Key="docs/missing.txt"), "404", "head missing -> 404")

    # empty object
    s3.put_object(Bucket=b, Key="empty", Body=b"")
    check(s3.get_object(Bucket=b, Key="empty")["ContentLength"] == 0, "empty object")

    # special keys
    for key in ["with space.txt", "üñïçødé.txt", "a//b.txt", "sym?bols&=%2F.txt", "dir/"]:
        s3.put_object(Bucket=b, Key=key, Body=key.encode())
        check(s3.get_object(Bucket=b, Key=key)["Body"].read() == key.encode(), f"weird key roundtrip: {key}")

    # --- range requests ---
    rg = s3.get_object(Bucket=b, Key="docs/readme.txt", Range="bytes=0-99")
    check(rg["Body"].read() == body[:100] and rg["ResponseMetadata"]["HTTPStatusCode"] == 206, "range bytes=0-99")
    rg = s3.get_object(Bucket=b, Key="docs/readme.txt", Range="bytes=-50")
    check(rg["Body"].read() == body[-50:], "suffix range bytes=-50")
    rg = s3.get_object(Bucket=b, Key="docs/readme.txt", Range=f"bytes={len(body)-10}-")
    check(rg["Body"].read() == body[-10:], "open-ended range")
    expect_error(lambda: s3.get_object(Bucket=b, Key="docs/readme.txt", Range=f"bytes={len(body)+5}-"), "InvalidRange", "range beyond size -> 416")

    # --- listing v2 ---
    for i in range(5):
        s3.put_object(Bucket=b, Key=f"list/a{i}.txt", Body=str(i).encode())
    s3.put_object(Bucket=b, Key="list/sub/deep.txt", Body=b"x")
    r = s3.list_objects_v2(Bucket=b, Prefix="list/", Delimiter="/")
    keys = sorted(c["Key"] for c in r.get("Contents", []))
    prefixes = [p["Prefix"] for p in r.get("CommonPrefixes", [])]
    check(keys == [f"list/a{i}.txt" for i in range(5)], "list v2 keys", str(keys))
    check(prefixes == ["list/sub/"], "list v2 common prefixes", str(prefixes))
    check(r["KeyCount"] == 6, "list v2 KeyCount")

    # pagination
    page1 = s3.list_objects_v2(Bucket=b, MaxKeys=2)
    check(len(page1["Contents"]) == 2 and page1["IsTruncated"], "list v2 page1 truncated")
    page2 = s3.list_objects_v2(Bucket=b, MaxKeys=2, ContinuationToken=page1["NextContinuationToken"])
    check(len(page2["Contents"]) == 2 and page2["Contents"][0]["Key"] > page1["Contents"][-1]["Key"], "list v2 page2 via token")
    paginator = s3.get_paginator("list_objects_v2")
    all_keys = []
    for page in paginator.paginate(Bucket=b, PaginationConfig={"PageSize": 3}):
        all_keys += [c["Key"] for c in page.get("Contents", [])]
    check(len(all_keys) == len(set(all_keys)) and len(all_keys) >= 10, "paginator returns unique keys", str(len(all_keys)))

    # listing v1
    v1 = s3.list_objects(Bucket=b, Prefix="list/", MaxKeys=3)
    check(len(v1["Contents"]) == 3 and v1.get("IsTruncated"), "list v1 page")
    v1b = s3.list_objects(Bucket=b, Prefix="list/", Marker=v1["NextMarker"])
    check(v1b["Contents"][0]["Key"] > v1["Contents"][-1]["Key"], "list v1 marker")

    # start-after
    sa = s3.list_objects_v2(Bucket=b, StartAfter="list/a2.txt", Prefix="list/")
    check(all(c["Key"] > "list/a2.txt" for c in sa["Contents"]), "list v2 start-after")

    # --- copy ---
    s3.copy_object(Bucket=b, CopySource=f"{b}/docs/readme.txt", Key="docs/copy.txt")
    check(s3.get_object(Bucket=b, Key="docs/copy.txt")["Body"].read() == body, "copy_object content")
    check(s3.head_object(Bucket=b, Key="docs/copy.txt")["ContentType"] == "text/plain", "copy_object keeps content type")
    expect_error(lambda: s3.copy_object(Bucket=b, CopySource=f"{b}/no/such.txt", Key="x"), "NoSuchKey", "copy missing source -> NoSuchKey")

    # --- batch delete ---
    r = s3.delete_objects(Bucket=b, Delete={"Objects": [{"Key": "list/a0.txt"}, {"Key": "list/a1.txt", "VersionId": "bogus"}]})
    check(len(r["Deleted"]) == 2, "delete_objects batch", str(r))
    expect_error(lambda: s3.get_object(Bucket=b, Key="list/a0.txt"), "NoSuchKey", "batch-deleted key gone")

    # single delete
    s3.delete_object(Bucket=b, Key="list/a2.txt")
    expect_error(lambda: s3.get_object(Bucket=b, Key="list/a2.txt"), "NoSuchKey", "single delete")

    # --- multipart ---
    from boto3.s3.transfer import TransferConfig
    tc = TransferConfig(multipart_threshold=5 * 1024 * 1024, multipart_chunksize=5 * 1024 * 1024)
    big = os.urandom(12 * 1024 * 1024 + 123)
    import io as _io
    s3.upload_fileobj(Bucket=b, Key="big/random.bin", Fileobj=_io.BytesIO(big), Config=tc)
    check(s3.get_object(Bucket=b, Key="big/random.bin")["Body"].read() == big, "multipart upload via transfer manager")
    check("-" in s3.head_object(Bucket=b, Key="big/random.bin")["ETag"], "multipart etag has -N suffix")

    # manual multipart flow
    mu = s3.create_multipart_upload(Bucket=b, Key="mp/manual.txt", ContentType="text/x-manual")
    uid = mu["UploadId"]
    p1 = b"A" * (5 * 1024 * 1024)
    p2 = b"B" * 100
    e1 = s3.upload_part(Bucket=b, Key="mp/manual.txt", UploadId=uid, PartNumber=1, Body=p1)["ETag"]
    e2 = s3.upload_part(Bucket=b, Key="mp/manual.txt", UploadId=uid, PartNumber=2, Body=p2)["ETag"]
    lp = s3.list_parts(Bucket=b, Key="mp/manual.txt", UploadId=uid)
    check(len(lp["Parts"]) == 2 and lp["Parts"][0]["PartNumber"] == 1, "list_parts")
    comp = s3.complete_multipart_upload(Bucket=b, Key="mp/manual.txt", UploadId=uid,
        MultipartUpload={"Parts": [{"PartNumber": 1, "ETag": e1}, {"PartNumber": 2, "ETag": e2}]})
    check(comp["ETag"].endswith("-2\""), "complete_multipart_upload etag", comp["ETag"])
    check(s3.get_object(Bucket=b, Key="mp/manual.txt")["Body"].read() == p1 + p2, "manual multipart content")
    check(s3.head_object(Bucket=b, Key="mp/manual.txt")["ContentType"] == "text/x-manual", "multipart content type")

    # upload_part_copy
    mu2 = s3.create_multipart_upload(Bucket=b, Key="mp/copied.txt")
    uid2 = mu2["UploadId"]
    pc = s3.upload_part_copy(Bucket=b, Key="mp/copied.txt", UploadId=uid2, PartNumber=1,
                             CopySource=f"{b}/docs/readme.txt")
    s3.complete_multipart_upload(Bucket=b, Key="mp/copied.txt", UploadId=uid2,
                                 MultipartUpload={"Parts": [{"PartNumber": 1, "ETag": pc["CopyPartResult"]["ETag"]}]})
    check(s3.get_object(Bucket=b, Key="mp/copied.txt")["Body"].read() == body, "upload_part_copy content")

    # abort + list uploads
    mu3 = s3.create_multipart_upload(Bucket=b, Key="mp/aborted.txt")
    lu = s3.list_multipart_uploads(Bucket=b, Prefix="mp/")
    check(any(u["UploadId"] == mu3["UploadId"] for u in lu.get("Uploads", [])), "list_multipart_uploads shows pending")
    s3.abort_multipart_upload(Bucket=b, Key="mp/aborted.txt", UploadId=mu3["UploadId"])
    lu = s3.list_multipart_uploads(Bucket=b, Prefix="mp/")
    check(not any(u["UploadId"] == mu3["UploadId"] for u in lu.get("Uploads", [])), "abort removes upload")
    expect_error(lambda: s3.abort_multipart_upload(Bucket=b, Key="mp/aborted.txt", UploadId=mu3["UploadId"]), "NoSuchUpload", "abort twice -> NoSuchUpload")

    # --- versioning ---
    s3.put_bucket_versioning(Bucket=b, VersioningConfiguration={"Status": "Enabled"})
    check(s3.get_bucket_versioning(Bucket=b)["Status"] == "Enabled", "get_bucket_versioning Enabled")
    vids = []
    for i in range(3):
        vids.append(s3.put_object(Bucket=b, Key="ver/obj.txt", Body=f"version-{i}".encode())["VersionId"])
    check(all(v and v != "null" for v in vids), "version ids assigned")
    lv = s3.list_object_versions(Bucket=b, Prefix="ver/")
    vers = [v["VersionId"] for v in lv["Versions"]]
    check(set(vids) <= set(vers), "list_object_versions contains all versions")
    latest = [v for v in lv["Versions"] if v["IsLatest"]][0]
    check(latest["VersionId"] == vids[-1], "IsLatest is the last put")
    check(s3.get_object(Bucket=b, Key="ver/obj.txt")["Body"].read() == b"version-2", "latest get")
    check(s3.get_object(Bucket=b, Key="ver/obj.txt", VersionId=vids[0])["Body"].read() == b"version-0", "get by versionId")
    check(s3.head_object(Bucket=b, Key="ver/obj.txt", VersionId=vids[1])["ContentLength"] == len("version-1"), "head by versionId")

    # delete without versionId -> delete marker
    dm = s3.delete_object(Bucket=b, Key="ver/obj.txt")
    check(dm.get("DeleteMarker") is True and dm.get("VersionId"), "delete creates delete marker")
    expect_error(lambda: s3.get_object(Bucket=b, Key="ver/obj.txt"), "NoSuchKey", "get after delete marker -> NoSuchKey")
    lv = s3.list_object_versions(Bucket=b, Prefix="ver/")
    check(any(d["VersionId"] == dm["VersionId"] for d in lv.get("DeleteMarkers", [])), "delete marker listed")
    # copy a specific version
    s3.copy_object(Bucket=b, CopySource=f"{b}/ver/obj.txt?versionId={vids[0]}", Key="ver/copy.txt")
    check(s3.get_object(Bucket=b, Key="ver/copy.txt")["Body"].read() == b"version-0", "copy specific version")
    # remove delete marker -> object visible again
    s3.delete_object(Bucket=b, Key="ver/obj.txt", VersionId=dm["VersionId"])
    check(s3.get_object(Bucket=b, Key="ver/obj.txt")["Body"].read() == b"version-2", "delete marker removed restores latest")
    # delete specific version permanently
    s3.delete_object(Bucket=b, Key="ver/obj.txt", VersionId=vids[0])
    expect_error(lambda: s3.get_object(Bucket=b, Key="ver/obj.txt", VersionId=vids[0]), "NoSuchVersion", "deleted version -> NoSuchVersion")
    # suspend -> null version
    s3.put_bucket_versioning(Bucket=b, VersioningConfiguration={"Status": "Suspended"})
    nv = s3.put_object(Bucket=b, Key="ver/obj.txt", Body=b"suspended-write")
    check(nv["VersionId"] == "null", "suspended put writes null version", str(nv.get("VersionId")))
    check(s3.get_object(Bucket=b, Key="ver/obj.txt")["Body"].read() == b"suspended-write", "null version overwrites")
    s3.put_bucket_versioning(Bucket=b, VersioningConfiguration={"Status": "Enabled"})

    # --- presigned URL ---
    url = s3.generate_presigned_url("get_object", Params={"Bucket": b, "Key": "docs/readme.txt"}, ExpiresIn=300)
    with urllib.request.urlopen(url) as resp:
        check(resp.read() == body and resp.status == 200, "presigned get_object URL works")
    url_put = s3.generate_presigned_url("put_object", Params={"Bucket": b, "Key": "presigned.txt"}, ExpiresIn=300)
    req = urllib.request.Request(url_put, data=b"via-presigned", method="PUT")
    with urllib.request.urlopen(req) as resp:
        check(resp.status == 200, "presigned put_object URL works")
    check(s3.get_object(Bucket=b, Key="presigned.txt")["Body"].read() == b"via-presigned", "presigned put content")

    # --- wrong bucket ---
    expect_error(lambda: s3.list_objects_v2(Bucket="other-bucket"), "NoSuchBucket", "wrong bucket -> NoSuchBucket")

    # --- web UI ---
    with urllib.request.urlopen(args.endpoint + "/ui/") as resp:
        html = resp.read().decode()
        check(resp.status == 200 and "miniS3" in html and "htmx.min.js" in html, "UI index page")
    with urllib.request.urlopen(args.endpoint + "/ui/static/htmx.min.js") as resp:
        check(resp.status == 200 and len(resp.read()) > 10000, "htmx vendored")
    with urllib.request.urlopen(args.endpoint + "/ui/browse?prefix=docs/&delimiter=/") as resp:
        frag = resp.read().decode()
        check("readme.txt" in frag, "UI browse fragment lists objects")
    # UI upload
    boundary = "----minis3test"
    body_ui = (
        f"--{boundary}\r\nContent-Disposition: form-data; name=\"prefix\"\r\n\r\n\r\n"
        f"--{boundary}\r\nContent-Disposition: form-data; name=\"file\"; filename=\"ui-upload.txt\"\r\n"
        f"Content-Type: text/plain\r\n\r\nhello from UI\r\n--{boundary}--\r\n"
    ).encode()
    req = urllib.request.Request(args.endpoint + "/ui/upload", data=body_ui, method="POST",
                                 headers={"Content-Type": f"multipart/form-data; boundary={boundary}", "HX-Request": "true"})
    with urllib.request.urlopen(req) as resp:
        check(resp.status == 200 and "ui-upload.txt" in resp.read().decode(), "UI upload returns listing")
    check(s3.get_object(Bucket=b, Key="ui-upload.txt")["Body"].read() == b"hello from UI", "UI upload stored via S3 API")
    with urllib.request.urlopen(args.endpoint + "/ui/download?key=ui-upload.txt") as resp:
        check(resp.read() == b"hello from UI" and "attachment" in resp.headers.get("Content-Disposition", ""), "UI download")
    with urllib.request.urlopen(args.endpoint + f"/ui/versions?key=ver/obj.txt") as resp:
        check("VersionId" in resp.read().decode(), "UI versions fragment")
    with urllib.request.urlopen(args.endpoint + "/ui/versioning") as resp:
        check("versioning" in resp.read().decode(), "UI versioning fragment")

    print(f"\nALL {PASS} CHECKS PASSED")


if __name__ == "__main__":
    main()
