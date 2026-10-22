#!/bin/sh
# Records the responses the tests replay, from bin/moto's server.
set -eu
cd "$(dirname "$0")"
port=18399
../../../bin/moto/bin/moto_server -H 127.0.0.1 -p $port >/dev/null 2>&1 &
trap 'kill $!' EXIT
sleep 3
# rec FILE METHOD PATH [BODY] saves the response to FILE.http.
rec() {
	file=$1 method=$2 path=$3
	shift 3
	if [ "$method" = HEAD ]; then set -- -I; else set -- -X "$method" ${1:+--data-binary "$1"}; fi
	curl -sSi "$@" -H 'Content-Type: application/octet-stream' \
		-H 'Authorization: AWS4-HMAC-SHA256 Credential=test/20261021/us-east-1/s3/aws4_request, SignedHeaders=host, Signature=0' \
		"http://127.0.0.1:$port$path" |
		tr -d '\r' | grep -iv '^\(Date\|Transfer-Encoding\|x-amzn-requestid\|Server\):' >"$file.http"
}
# send METHOD PATH [BODY] prints the response's x-amz-version-id.
send() {
	rec send "$@"
	sed -n 's/^x-amz-version-id: //Ip' send.http
	rm send.http
}
rec create-bucket PUT /logs-bucket
rec put-tagging PUT '/logs-bucket?tagging' '<Tagging><TagSet><Tag><Key>team</Key><Value>a</Value></Tag></TagSet></Tagging>'
rec put-object PUT /logs-bucket/a/k.txt hello
rec head-object HEAD /logs-bucket/a/k.txt
rec list-buckets GET /
rec list-objects GET '/logs-bucket?list-type=2'
rec create-versioned PUT /data-bucket
rec put-versioning PUT '/data-bucket?versioning' '<VersioningConfiguration><Status>Enabled</Status></VersioningConfiguration>'
rec put-versioned PUT /data-bucket/k v1
rec put-versioned-again PUT /data-bucket/k v2
rec put-policy PUT '/data-bucket?policy' '{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::data-bucket/*"}]}'
rec multipart POST '/logs-bucket/big?uploads'
mkdir -p listing
for request in 'GET /' 'GET /logs-bucket?versioning' 'GET /logs-bucket?tagging' 'GET /logs-bucket?policy' 'GET /logs-bucket?versions' \
	'GET /data-bucket?versioning' 'GET /data-bucket?tagging' 'GET /data-bucket?policy' 'GET /data-bucket?versions'; do
	set -- $request
	rec "listing/$(echo "$1$2" | tr '/?=&' '____')" "$1" "$2"
done
rec delete-objects POST '/logs-bucket?delete' '<Delete><Object><Key>a/k.txt</Key></Object></Delete>'
rec delete-object DELETE /data-bucket/k
rec delete-bucket DELETE /logs-bucket
send PUT /kept-bucket >/dev/null
send PUT '/kept-bucket?versioning' '<VersioningConfiguration><Status>Enabled</Status></VersioningConfiguration>' >/dev/null
old=$(send PUT /kept-bucket/k v1)
send PUT /kept-bucket/k v2 >/dev/null
rec delete-objects-version POST '/kept-bucket?delete' "<Delete><Object><Key>k</Key><VersionId>$old</VersionId></Object></Delete>"
rec delete-objects-marker POST '/kept-bucket?delete' '<Delete><Object><Key>k</Key></Object></Delete>'
