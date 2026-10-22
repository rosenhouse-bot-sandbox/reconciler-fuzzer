#!/bin/sh
# Records the responses the tests replay, from bin/vault in dev mode.
set -eu
cd "$(dirname "$0")"
addr=127.0.0.1:18299
../../../bin/vault server -dev -dev-root-token-id=root -dev-listen-address=$addr >/dev/null 2>&1 &
trap 'kill $!' EXIT
sleep 2
# rec FILE METHOD PATH [BODY] saves the response to FILE.http.
rec() {
	file=$1 method=$2 path=$3
	shift 3
	curl -sSi -X "$method" -H 'X-Vault-Token: root' -H 'Content-Type: application/json' \
		${1:+-d "$1"} "http://$addr$path" | tr -d '\r' | grep -v '^\(Date\|Transfer-Encoding\):' >"$file.http"
}
rec sys-mounts GET /v1/sys/mounts
rec create POST /v1/secret/data/app/db '{"data":{"password":"p1"}}'
rec update PUT /v1/secret/data/app/db '{"data":{"password":"p2"}}'
rec get-data GET /v1/secret/data/app/db
rec get-metadata GET /v1/secret/metadata/app/db
rec list LIST /v1/secret/metadata/
rec create-top POST /v1/secret/data/top '{"data":{"k":"v"}}'
rec soft-delete DELETE /v1/secret/data/top
rec get-data-deleted GET /v1/secret/data/top
rec patch-metadata POST /v1/secret/metadata/top '{"custom_metadata":{"owner":"me"}}'
rec unmapped GET /v1/secret/config
mkdir -p listing
for request in 'GET /v1/sys/mounts' 'GET /v1/secret/metadata/?list=true' 'GET /v1/secret/metadata/app/?list=true' \
	'GET /v1/secret/metadata/app/db' 'GET /v1/secret/data/app/db?version=2' 'GET /v1/secret/metadata/top' \
	'GET /v1/secret/data/top?version=1'; do
	set -- $request
	rec "listing/$(echo "$1$2" | tr '/?=&' '____')" "$1" "$2"
done
rec delete DELETE /v1/secret/metadata/app/db
