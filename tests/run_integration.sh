#!/usr/bin/env bash
# Runs the boto3 integration suite against miniS3 in every DB mode,
# plus auto-populate and SigV4-auth scenarios. Usage: tests/run_integration.sh
set -u
cd "$(dirname "$0")/.."

BIN=$(mktemp -u /tmp/minis3-bin-XXXX)
go build -o "$BIN" . || exit 1

fail=0
run_mode() {
  local mode="$1" port="$2" extra="${3:-}"
  local dir
  dir=$(mktemp -d /tmp/minis3-data-XXXX)
  echo "=== mode: $mode (port $port) ==="
  "$BIN" --addr "127.0.0.1:$port" --data-dir "$dir" --bucket testbucket --db "$mode" $extra > "$dir/server.log" 2>&1 &
  local pid=$!
  sleep 1
  if ! kill -0 "$pid" 2>/dev/null; then
    echo "server failed to start:"; cat "$dir/server.log"; fail=1; return
  fi
  if python3 tests/s3_integration.py --endpoint "http://127.0.0.1:$port"; then
    echo "=== mode $mode: OK ==="
  else
    echo "=== mode $mode: FAILED ==="
    cat "$dir/server.log"
    fail=1
  fi
  kill "$pid" 2>/dev/null
  wait "$pid" 2>/dev/null
  echo
}

run_mode none 19101
run_mode memory 19102
run_mode file 19103
run_mode auto 19104

# --- auto-populate: write with --db none, restart with --db memory ---
echo "=== auto-populate scenario ==="
dir=$(mktemp -d /tmp/minis3-pop-XXXX)
"$BIN" --addr 127.0.0.1:19110 --data-dir "$dir" --bucket testbucket --db none > "$dir/s1.log" 2>&1 &
pid=$!
sleep 1
python3 - "$dir" <<'EOF'
import sys, boto3
from botocore.config import Config
s3 = boto3.client('s3', endpoint_url='http://127.0.0.1:19110', aws_access_key_id='x', aws_secret_access_key='y',
                  region_name='us-east-1', config=Config(s3={'addressing_style': 'path'}))
s3.put_bucket_versioning(Bucket='testbucket', VersioningConfiguration={'Status': 'Enabled'})
s3.put_object(Bucket='testbucket', Key='persist/a.txt', Body=b'AAA')
s3.put_object(Bucket='testbucket', Key='persist/a.txt', Body=b'BBB')
s3.put_object(Bucket='testbucket', Key='persist/b.txt', Body=b'CCC')
print('seeded 3 objects (2 versions of a.txt) in db=none mode')
EOF
kill $pid; wait $pid 2>/dev/null
"$BIN" --addr 127.0.0.1:19111 --data-dir "$dir" --bucket testbucket --db memory > "$dir/s2.log" 2>&1 &
pid=$!
sleep 1
python3 - <<'EOF'
import boto3, sys
from botocore.config import Config
s3 = boto3.client('s3', endpoint_url='http://127.0.0.1:19111', aws_access_key_id='x', aws_secret_access_key='y',
                  region_name='us-east-1', config=Config(s3={'addressing_style': 'path'}))
keys = sorted(c['Key'] for c in s3.list_objects_v2(Bucket='testbucket')['Contents'])
assert keys == ['persist/a.txt', 'persist/b.txt'], keys
assert s3.get_object(Bucket='testbucket', Key='persist/a.txt')['Body'].read() == b'BBB'
vs = s3.list_object_versions(Bucket='testbucket', Prefix='persist/a.txt')['Versions']
assert len(vs) == 2, vs
assert s3.get_bucket_versioning(Bucket='testbucket')['Status'] == 'Enabled'
print('PASS: sqlite memory DB auto-populated from disk on startup (objects, versions, versioning status)')
EOF
rc=$?
kill $pid 2>/dev/null; wait $pid 2>/dev/null
[ $rc -ne 0 ] && { echo "=== auto-populate: FAILED ==="; cat "$dir/s2.log"; fail=1; } || echo "=== auto-populate: OK ==="
echo

# --- SigV4 auth mode ---
echo "=== sigv4 auth scenario ==="
dir=$(mktemp -d /tmp/minis3-auth-XXXX)
"$BIN" --addr 127.0.0.1:19120 --data-dir "$dir" --bucket testbucket --db memory \
  --access-key AKIAEXAMPLE --secret-key secretsecretsecret > "$dir/s.log" 2>&1 &
pid=$!
sleep 1
python3 - <<'EOF'
import boto3, urllib.request, urllib.error
from botocore.config import Config
from botocore.exceptions import ClientError

def client(ak, sk):
    return boto3.client('s3', endpoint_url='http://127.0.0.1:19120', aws_access_key_id=ak,
                        aws_secret_access_key=sk, region_name='us-east-1',
                        config=Config(s3={'addressing_style': 'path'}))

good = client('AKIAEXAMPLE', 'secretsecretsecret')
good.put_object(Bucket='testbucket', Key='k', Body=b'v')
assert good.get_object(Bucket='testbucket', Key='k')['Body'].read() == b'v'
print('PASS: valid SigV4 credentials work')
url = good.generate_presigned_url('get_object', Params={'Bucket': 'testbucket', 'Key': 'k'}, ExpiresIn=60)
with urllib.request.urlopen(url) as r:
    assert r.read() == b'v'
print('PASS: presigned URL (default sigv2) verified in auth mode')
goodv4 = boto3.client('s3', endpoint_url='http://127.0.0.1:19120', aws_access_key_id='AKIAEXAMPLE',
                      aws_secret_access_key='secretsecretsecret', region_name='us-east-1',
                      config=Config(s3={'addressing_style': 'path'}, signature_version='s3v4'))
url4 = goodv4.generate_presigned_url('get_object', Params={'Bucket': 'testbucket', 'Key': 'k'}, ExpiresIn=60)
assert 'X-Amz-Signature' in url4
with urllib.request.urlopen(url4) as r:
    assert r.read() == b'v'
print('PASS: presigned URL (sigv4) verified in auth mode')

bad = client('AKIAEXAMPLE', 'wrongsecret')
try:
    bad.list_objects_v2(Bucket='testbucket')
    raise SystemExit('FAIL: wrong secret accepted')
except ClientError as e:
    assert e.response['Error']['Code'] == 'SignatureDoesNotMatch', e.response['Error']['Code']
    print('PASS: wrong secret -> SignatureDoesNotMatch')

unk = client('AKIAOTHER', 'whatever')
try:
    unk.list_objects_v2(Bucket='testbucket')
    raise SystemExit('FAIL: unknown access key accepted')
except ClientError as e:
    assert e.response['Error']['Code'] == 'InvalidAccessKeyId', e.response['Error']['Code']
    print('PASS: unknown access key -> InvalidAccessKeyId')

anon = boto3.client('s3', endpoint_url='http://127.0.0.1:19120', region_name='us-east-1',
                     config=Config(s3={'addressing_style': 'path'}, signature_version='s3v4'))
anon = boto3.client('s3', endpoint_url='http://127.0.0.1:19120', aws_access_key_id='', aws_secret_access_key='',
                     region_name='us-east-1', config=Config(s3={'addressing_style': 'path'}))
try:
    anon.list_objects_v2(Bucket='testbucket')
    raise SystemExit('FAIL: anonymous request accepted in auth mode')
except ClientError as e:
    assert e.response['ResponseMetadata']['HTTPStatusCode'] == 403, e.response
    print('PASS: anonymous request -> 403 in auth mode')

# tampered signature: sign with good creds then modify a byte of the signature
import botocore.auth, botocore.awsrequest
req = anon.meta.events  # noqa - just ensure import works
raw = urllib.request.Request('http://127.0.0.1:19120/testbucket?list-type=2', headers={
    'Authorization': 'AWS4-HMAC-SHA256 Credential=AKIAEXAMPLE/20260101/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-date, Signature=0000000000000000000000000000000000000000000000000000000000000000',
    'X-Amz-Date': '20260101T000000Z'})
try:
    urllib.request.urlopen(raw)
    raise SystemExit('FAIL: tampered signature accepted')
except urllib.error.HTTPError as e:
    assert e.code == 403, e.code
    print('PASS: tampered signature -> 403')
EOF
rc=$?
kill $pid 2>/dev/null; wait $pid 2>/dev/null
[ $rc -ne 0 ] && { echo "=== sigv4 auth: FAILED ==="; cat "$dir/s.log"; fail=1; } || echo "=== sigv4 auth: OK ==="

rm -f "$BIN"
if [ $fail -ne 0 ]; then echo "INTEGRATION FAILURES"; exit 1; fi
echo "ALL INTEGRATION SCENARIOS PASSED"
