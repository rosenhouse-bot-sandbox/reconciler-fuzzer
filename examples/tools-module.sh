mkdir -p tools/reconciler-fuzzer
cd tools/reconciler-fuzzer
go mod init example.com/operator/tools/reconciler-fuzzer
go mod edit -go=1.26.0
GOWORK=off go get -tool github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/cmd/reconciler-fuzzer@latest
cd ../..
GOWORK=off go -C tools/reconciler-fuzzer build -o ../../bin/reconciler-fuzzer github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/cmd/reconciler-fuzzer
bin/reconciler-fuzzer version
