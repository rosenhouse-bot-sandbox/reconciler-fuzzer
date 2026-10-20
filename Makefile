# Tool and control-plane pins live here, one variable each. Bumping one is its
# own pull request.

ENVTEST_K8S_VERSION ?= 1.37.0
SETUP_ENVTEST_VERSION ?= v0.25.2
CONTROLLER_GEN_VERSION ?= v0.22.0
CERT_MANAGER_VERSION ?= v1.21.2
# The commit that tag names, and the sha256 of its CRD release asset. A tag can
# move, and a version label inside the asset cannot tell a patched file from
# the release.
CERT_MANAGER_COMMIT ?= 922a06aa49ee4bb802db268ef72a174af70edd32
CERT_MANAGER_CRDS_SHA256 ?= 262fef78478492cd35b73a1b227106b7802f9c056361d1f561d04b32d751337f
EXTERNAL_SECRETS_VERSION ?= v2.11.0
# The commit that tag names, and the sha256 of its CRD release asset. A tag can
# move, and this asset carries no version label at all, so the sha256 is the
# only thing that tells it from another.
EXTERNAL_SECRETS_COMMIT ?= e8f12e1f1646e0ad47966458023ff10c9577f2b0
EXTERNAL_SECRETS_CRDS_SHA256 ?= c427124642886d240af8f28d294714a168d920ef8e5d55db14600e00ed0cfb80
# The release index setup-envtest downloads from, pinned to a controller-tools tag.
ENVTEST_INDEX_URL ?= https://raw.githubusercontent.com/kubernetes-sigs/controller-tools/$(CONTROLLER_GEN_VERSION)/envtest-releases.yaml

# A hunt draws HUNT_SEEDS seeds from HUNT_FROM, which defaults to a fresh one.
# HUNT_TIMEOUT interrupts it before the nightly job's 60-minute timeout, so
# its finds are still reported.
HUNT_SEEDS ?= 20
HUNT_FROM ?= $(shell date +%s)
HUNT_TIMEOUT ?= 40m
HUNT_OUT := reconciler-fuzzer-out
# The toy's generated tier runs apart from the envtest tier.
GENERATED = TestGeneratedPlansWithFaultsPassB0|TestAGeneratedFailingPlanShrinks

# Project-local tool and asset directories. Both are git-ignored.
LOCALBIN := $(CURDIR)/bin
ENVTEST_ASSETS_DIR := $(LOCALBIN)/envtest
SETUP_ENVTEST := $(LOCALBIN)/setup-envtest
CERT_MANAGER_REPO := https://github.com/cert-manager/cert-manager.git
CERT_MANAGER_SRC := $(LOCALBIN)/cert-manager-src
CERT_MANAGER := $(LOCALBIN)/cert-manager-controller
# The stamp carries the pin, so bumping CERT_MANAGER_VERSION rebuilds.
CERT_MANAGER_STAMP := $(LOCALBIN)/cert-manager-$(CERT_MANAGER_VERSION).built
CERT_MANAGER_CRDS := examples/cert-manager/crds/cert-manager.crds.yaml
EXTERNAL_SECRETS_REPO := https://github.com/external-secrets/external-secrets.git
EXTERNAL_SECRETS_SRC := $(LOCALBIN)/external-secrets-src
EXTERNAL_SECRETS := $(LOCALBIN)/external-secrets
EXTERNAL_SECRETS_STAMP := $(LOCALBIN)/external-secrets-$(EXTERNAL_SECRETS_VERSION).built
EXTERNAL_SECRETS_CRDS := examples/external-secrets/crds/external-secrets.yaml

# Let go fetch the toolchain go.mod asks for.
GOTOOLCHAIN ?= auto
export GOTOOLCHAIN

# Prints the KUBEBUILDER_ASSETS directory for the pinned version.
ENVTEST_USE := $(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION) --index $(ENVTEST_INDEX_URL) --bin-dir $(ENVTEST_ASSETS_DIR) -p path

.PHONY: help
help:
	@echo "Targets:"
	@echo "  setup                             Download modules and install the envtest control plane."
	@echo "  assets-path                       Print the KUBEBUILDER_ASSETS directory and nothing else."
	@echo "  build                             Build bin/reconciler-fuzzer and bin/toy-widget."
	@echo "  generate                          Write the toy's deepcopy code and CRD YAML."
	@echo "  verify-generate                   Fail if a generated file is stale."
	@echo "  fmt                               Fail if any file needs gofmt."
	@echo "  vet                               Run go vet, envtest files included."
	@echo "  test                              Run the unit tests, the size test and the bug-matrix check."
	@echo "  test-envtest                      Run the envtest tests, the toy's gate included."
	@echo "  test-generated                    Run the toy's generated plans under B0, and shrink one under B3."
	@echo "  bug-matrix                        Rewrite docs/bug-matrix.md from the toy's gate table."
	@echo "  test-example-cert-manager         Run the cert-manager plans, seeds and negative control."
	@echo "  test-example-external-secrets     Run the external-secrets plans, seeds and negative control."
	@echo "  hunt-cert-manager                 Hunt cert-manager on HUNT_SEEDS seeds from HUNT_FROM, for at most HUNT_TIMEOUT."
	@echo "  hunt-external-secrets             Hunt external-secrets on HUNT_SEEDS seeds from HUNT_FROM, for at most HUNT_TIMEOUT."
	@echo "  cert-manager                      Clone and build the pinned cert-manager controller."
	@echo "  cert-manager-crds                 Refetch the pinned cert-manager CRD release asset."
	@echo "  cert-manager-version              Print CERT_MANAGER_VERSION and nothing else."
	@echo "  verify-cert-manager-pin           Fail if the example's CRDs or the upstream tag drifted from the pin."
	@echo "  verify-cert-manager-port          Fail if port 9403, where cert-manager listens, is bound."
	@echo "  external-secrets                  Clone and build the pinned external-secrets controller."
	@echo "  external-secrets-crds             Refetch the pinned external-secrets CRD release asset."
	@echo "  external-secrets-version          Print EXTERNAL_SECRETS_VERSION and nothing else."
	@echo "  verify-external-secrets-pin       Fail if the example's CRDs or the upstream tag drifted from the pin."

.PHONY: setup
setup: $(SETUP_ENVTEST)
	go mod download
	@$(ENVTEST_USE) >/dev/null
	@echo "The envtest control plane for Kubernetes $(ENVTEST_K8S_VERSION) is installed."

# Silent, so that assets-path prints only the path even when this runs first.
$(SETUP_ENVTEST):
	@GOBIN=$(LOCALBIN) go install sigs.k8s.io/controller-runtime/tools/setup-envtest@$(SETUP_ENVTEST_VERSION)

.PHONY: assets-path
assets-path: $(SETUP_ENVTEST)
	@$(ENVTEST_USE)

.PHONY: build
build:
	go build -o bin/reconciler-fuzzer ./cmd/reconciler-fuzzer
	go build -o bin/toy-widget ./targets/toy-widget

.PHONY: generate
generate:
	go run sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_GEN_VERSION) \
		object paths=./targets/toy-widget/api/... \
		crd paths=./targets/toy-widget/api/... output:crd:artifacts:config=targets/toy-widget/crds

.PHONY: verify-generate
verify-generate: generate
	@stale=$$(git status --porcelain -- targets/toy-widget/api/v1/zz_generated.deepcopy.go targets/toy-widget/crds); \
	if [ -n "$$stale" ]; then \
		echo "Generated files are out of date. Run 'make generate' and commit the result:"; \
		echo "$$stale"; \
		exit 1; \
	fi

.PHONY: fmt
fmt:
	@files=$$(gofmt -l $$(git ls-files -co --exclude-standard '*.go')); \
	if [ -n "$$files" ]; then \
		echo "These files need gofmt:"; \
		echo "$$files"; \
		exit 1; \
	fi

.PHONY: vet
vet:
	go vet -tags envtest ./...

.PHONY: test
test:
	go test -count=1 ./...

.PHONY: test-envtest
test-envtest: setup
	KUBEBUILDER_ASSETS="$$($(ENVTEST_USE))" go test -tags envtest -count=1 -p 2 -parallel 3 -timeout 20m -skip '$(GENERATED)' ./...

.PHONY: test-generated
test-generated: setup
	KUBEBUILDER_ASSETS="$$($(ENVTEST_USE))" go test -tags envtest -count=1 -p 2 -parallel 3 -timeout 30m -run '$(GENERATED)' ./targets/toy-widget

.PHONY: bug-matrix
bug-matrix:
	go test -count=1 -run TestBugMatrixIsCurrent ./targets/toy-widget -args -update

# Each example runs its plans and the seeds its seeds file lists, then its
# negative control. EXAMPLE_SEEDS replaces the seeds file.
.PHONY: test-example-cert-manager
test-example-cert-manager: verify-cert-manager-pin verify-cert-manager-port setup build cert-manager
	KUBEBUILDER_ASSETS="$$($(ENVTEST_USE))" examples/test.sh cert-manager \
		plans/issue.json example-tls --launch-arg --enable-certificate-owner-ref=false

.PHONY: test-example-external-secrets
test-example-external-secrets: verify-external-secrets-pin setup build external-secrets
	KUBEBUILDER_ASSETS="$$($(ENVTEST_USE))" examples/test.sh external-secrets \
		controls/orphan.json example-secret

# A hunt writes summary.md and each failing seed's run under
# reconciler-fuzzer-out/hunt-<name>. make exits 2 on any failure, so the hunt's
# own exit code, 1 on a find and 2 on an error, goes to exit-status there.
define hunt
	@rm -rf $(HUNT_OUT)/hunt-$(1) && mkdir -p $(HUNT_OUT)/hunt-$(1)
	@status=0; KUBEBUILDER_ASSETS="$$($(ENVTEST_USE))" timeout --preserve-status -s INT -k 5m $(HUNT_TIMEOUT) \
		./bin/reconciler-fuzzer hunt \
		--config examples/$(1)/config.yaml --seeds $(HUNT_SEEDS) --from $(HUNT_FROM) \
		--out $(HUNT_OUT)/hunt-$(1) || status=$$?; \
	echo $$status >$(HUNT_OUT)/hunt-$(1)/exit-status; \
	exit $$status
endef

.PHONY: hunt-cert-manager
hunt-cert-manager: verify-cert-manager-pin verify-cert-manager-port setup build cert-manager
	$(call hunt,cert-manager)

.PHONY: hunt-external-secrets
hunt-external-secrets: verify-external-secrets-pin setup build external-secrets
	$(call hunt,external-secrets)

# cert-manager's cmd/controller is a nested module with a replace directive and
# no tag of its own, so go install cannot reach it.
.PHONY: cert-manager
cert-manager: $(CERT_MANAGER)

# The binary is the target, so deleting it rebuilds. The stamp carries the pin,
# so bumping CERT_MANAGER_VERSION rebuilds. A build clones afresh and deletes
# the clone, so gofmt -l . sees only this repository.
$(CERT_MANAGER): $(CERT_MANAGER_STAMP)
	rm -rf $(CERT_MANAGER_SRC)
	git clone --depth 1 --branch $(CERT_MANAGER_VERSION) $(CERT_MANAGER_REPO) $(CERT_MANAGER_SRC)
	@cloned=$$(git -C $(CERT_MANAGER_SRC) rev-parse HEAD); \
	if [ "$$cloned" != "$(CERT_MANAGER_COMMIT)" ]; then \
		echo "$(CERT_MANAGER_VERSION) is $$cloned, and the pin is $(CERT_MANAGER_COMMIT). The tag moved."; \
		exit 1; \
	fi
	cd $(CERT_MANAGER_SRC)/cmd/controller && go build -o $@ .
	rm -rf $(CERT_MANAGER_SRC)

$(CERT_MANAGER_STAMP):
	rm -f $(LOCALBIN)/cert-manager-*.built
	touch $@

# CI keys its controller cache on this.
.PHONY: cert-manager-version
cert-manager-version:
	@echo $(CERT_MANAGER_VERSION)

# Bumping the pin needs this and a commit.
.PHONY: cert-manager-crds
cert-manager-crds:
	curl -fsSL -o $(CERT_MANAGER_CRDS) \
		https://github.com/cert-manager/cert-manager/releases/download/$(CERT_MANAGER_VERSION)/cert-manager.crds.yaml
	@echo "CERT_MANAGER_CRDS_SHA256 ?= $$(sha256sum $(CERT_MANAGER_CRDS) | cut -d' ' -f1)"

# verify-tag-pin holds an upstream tag to its pinned commit: (repository URL,
# tag, pinned commit, the variable holding it). CI's cache skips the build's
# own check, so a moved tag fails here. An annotated tag resolves through its
# peeled ^{} ref, and a lightweight one through the ref itself.
define verify-tag-pin
	@refs=$$(git ls-remote --tags $(1) '$(2)' '$(2)^{}') \
		|| { echo "git ls-remote could not reach $(1) to resolve $(2)."; exit 1; }; \
	commit=$$(echo "$$refs" | awk '$$2 == "refs/tags/$(2)^{}" {print $$1}'); \
	[ -n "$$commit" ] || commit=$$(echo "$$refs" | awk '$$2 == "refs/tags/$(2)" {print $$1}'); \
	if [ -z "$$commit" ]; then \
		echo "$(1) carries no tag $(2), which $(4) pins to $(3)."; \
		exit 1; \
	fi; \
	if [ "$$commit" != "$(3)" ]; then \
		echo "$(2) is $$commit upstream, and $(4) is $(3). The tag moved."; \
		exit 1; \
	fi
endef

.PHONY: verify-cert-manager-pin
verify-cert-manager-pin:
	@grep -q 'app.kubernetes.io/version: "$(CERT_MANAGER_VERSION)"' $(CERT_MANAGER_CRDS) \
		|| { echo "$(CERT_MANAGER_CRDS) is not the $(CERT_MANAGER_VERSION) release asset. Run 'make cert-manager-crds'."; exit 1; }
	@echo "$(CERT_MANAGER_CRDS_SHA256)  $(CERT_MANAGER_CRDS)" | sha256sum -c --status - \
		|| { echo "$(CERT_MANAGER_CRDS) hashes to $$(sha256sum $(CERT_MANAGER_CRDS) | cut -d' ' -f1). Refetch it, and pin the digest 'make cert-manager-crds' prints."; exit 1; }
	$(call verify-tag-pin,$(CERT_MANAGER_REPO),$(CERT_MANAGER_VERSION),$(CERT_MANAGER_COMMIT),CERT_MANAGER_COMMIT)

# cert-manager's healthz port is fixed, so its runs cannot overlap.
.PHONY: verify-cert-manager-port
verify-cert-manager-port:
	@if ! command -v lsof >/dev/null; then \
		echo "lsof is missing, so nothing checked whether port 9403 is free." >&2; \
	elif lsof -nP -iTCP:9403 -sTCP:LISTEN >/dev/null; then \
		echo "port 9403 is bound. cert-manager listens there, so its runs cannot overlap." >&2; \
		exit 1; \
	fi

# external-secrets' main package is the repository root, with replace
# directives onto in-tree modules, so go install cannot reach it either. The
# fake provider registers itself only under its build tag.
.PHONY: external-secrets
external-secrets: $(EXTERNAL_SECRETS)

$(EXTERNAL_SECRETS): $(EXTERNAL_SECRETS_STAMP)
	rm -rf $(EXTERNAL_SECRETS_SRC)
	git clone --depth 1 --branch $(EXTERNAL_SECRETS_VERSION) $(EXTERNAL_SECRETS_REPO) $(EXTERNAL_SECRETS_SRC)
	@cloned=$$(git -C $(EXTERNAL_SECRETS_SRC) rev-parse HEAD); \
	if [ "$$cloned" != "$(EXTERNAL_SECRETS_COMMIT)" ]; then \
		echo "$(EXTERNAL_SECRETS_VERSION) is $$cloned, and the pin is $(EXTERNAL_SECRETS_COMMIT). The tag moved."; \
		exit 1; \
	fi
	cd $(EXTERNAL_SECRETS_SRC) && go build -tags fake -o $@ .
	rm -rf $(EXTERNAL_SECRETS_SRC)

$(EXTERNAL_SECRETS_STAMP):
	rm -f $(LOCALBIN)/external-secrets-*.built
	touch $@

.PHONY: external-secrets-version
external-secrets-version:
	@echo $(EXTERNAL_SECRETS_VERSION)

.PHONY: external-secrets-crds
external-secrets-crds:
	curl -fsSL -o $(EXTERNAL_SECRETS_CRDS) \
		https://github.com/external-secrets/external-secrets/releases/download/$(EXTERNAL_SECRETS_VERSION)/external-secrets.yaml
	@echo "EXTERNAL_SECRETS_CRDS_SHA256 ?= $$(sha256sum $(EXTERNAL_SECRETS_CRDS) | cut -d' ' -f1)"

# The asset carries no version label, so its sha256 is what holds it to the pin.
.PHONY: verify-external-secrets-pin
verify-external-secrets-pin:
	@echo "$(EXTERNAL_SECRETS_CRDS_SHA256)  $(EXTERNAL_SECRETS_CRDS)" | sha256sum -c --status - \
		|| { echo "$(EXTERNAL_SECRETS_CRDS) hashes to $$(sha256sum $(EXTERNAL_SECRETS_CRDS) | cut -d' ' -f1). Refetch it, and pin the digest 'make external-secrets-crds' prints."; exit 1; }
	$(call verify-tag-pin,$(EXTERNAL_SECRETS_REPO),$(EXTERNAL_SECRETS_VERSION),$(EXTERNAL_SECRETS_COMMIT),EXTERNAL_SECRETS_COMMIT)
