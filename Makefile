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
# An OSS Vault release and the sha256 of its linux_amd64 zip, from its SHA256SUMS.
VAULT_VERSION ?= 2.1.2
VAULT_SHA256 ?= 873bbdac35ca4b0c3e2886c41991ca1272fc452826208d5499f32d12cd2e76e1
# moto 5.2.3 and its dependencies, each pinned by hash.
MOTO_REQUIREMENTS := requirements-moto.txt
# The release index setup-envtest downloads from, pinned to a controller-tools tag.
ENVTEST_INDEX_URL ?= https://raw.githubusercontent.com/kubernetes-sigs/controller-tools/$(CONTROLLER_GEN_VERSION)/envtest-releases.yaml

# A hunt draws HUNT_SEEDS seeds from HUNT_FROM, which defaults to a fresh one.
# HUNT_TIMEOUT interrupts it before the nightly job's 60-minute timeout, so
# its finds are still reported.
HUNT_SEEDS ?= 20
HUNT_FROM ?= $(shell date +%s)
HUNT_TIMEOUT ?= 40m
HUNT_OUT := reconciler-fuzzer-out
# A repro runs each plan in REPRO_PLANS, or else the example's bugs/ plans,
# REPRO_RUNS times.
REPRO_RUNS ?= 20
# Each directory in targets/ with a main.go is a toy.
TOYS := $(patsubst targets/%/main.go,%,$(sort $(wildcard targets/*/main.go)))
TOY_PACKAGES := $(TOYS:%=./targets/%)
# The toys' generated tiers run apart from the envtest tier.
GENERATED = TestGeneratedPlansWithFaultsPassB0|TestAGeneratedFailingPlanShrinks|TestGeneratedBucketPlansWithCloudFaultsPassB0

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
# The build tags pick the providers external-secrets registers.
EXTERNAL_SECRETS_TAGS := vault
EXTERNAL_SECRETS_BUILD := $(EXTERNAL_SECRETS_VERSION)-$(EXTERNAL_SECRETS_TAGS)
EXTERNAL_SECRETS_STAMP := $(LOCALBIN)/external-secrets-$(EXTERNAL_SECRETS_BUILD).built
EXTERNAL_SECRETS_CRDS := examples/external-secrets/crds/external-secrets.yaml
VAULT := $(LOCALBIN)/vault
VAULT_STAMP := $(LOCALBIN)/vault-$(VAULT_VERSION).installed
MOTO_VENV := $(LOCALBIN)/moto
MOTO_SERVER := $(MOTO_VENV)/bin/moto_server
MOTO_STAMP := $(LOCALBIN)/moto-$(shell sha256sum $(MOTO_REQUIREMENTS) | cut -c1-16).installed

# Let go fetch the toolchain go.mod asks for.
GOTOOLCHAIN ?= auto
export GOTOOLCHAIN

# Prints the KUBEBUILDER_ASSETS directory for the pinned version.
ENVTEST_USE := $(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION) --index $(ENVTEST_INDEX_URL) --bin-dir $(ENVTEST_ASSETS_DIR) -p path

.PHONY: help
help::
	@echo "Targets:"
	@echo "  setup                             Download modules, and install the envtest control plane, Vault and moto."
	@echo "  vault                             Download the pinned, checksummed Vault to bin/vault."
	@echo "  moto                              Install the pinned moto server into the bin/moto venv."
	@echo "  assets-path                       Print the KUBEBUILDER_ASSETS directory and nothing else."
	@echo "  build                             Build bin/reconciler-fuzzer, bin/crudfake and bin/<toy> for each toy in targets/."
	@echo "  generate                          Write the toys' deepcopy code and CRD YAML."
	@echo "  generate-<toy>                    Write one toy's deepcopy code and CRD YAML."
	@echo "  verify-generate                   Fail if a generated file is stale."
	@echo "  fmt                               Fail if any file needs gofmt."
	@echo "  vet                               Run go vet, envtest files included."
	@echo "  test                              Run the unit tests, the size test and the bug-matrix check."
	@echo "  test-envtest                      Run the envtest tests, the toys' gates included."
	@echo "  test-generated                    Run the toys' generated plans under B0, and shrink one under B3."
	@echo "  bug-matrix                        Rewrite docs/bug-matrix.md from the toys' gate tables."
	@echo "  test-example-cert-manager         Run the cert-manager plans, seeds, negative control and #63 plan."
	@echo "  test-example-external-secrets     Run the external-secrets plans, seeds and negative controls, rotation included."
	@echo "  hunt-cert-manager                 Hunt cert-manager on HUNT_SEEDS seeds from HUNT_FROM, for at most HUNT_TIMEOUT."
	@echo "  repro-cert-manager                Run each REPRO_PLANS plan, cert-manager's bugs/ by default, REPRO_RUNS times, and print its rate."
	@echo "  repro-external-secrets            Run each REPRO_PLANS plan, external-secrets' bugs/ by default, REPRO_RUNS times, and print its rate."
	@echo "  hunt-external-secrets             Hunt external-secrets on HUNT_SEEDS seeds from HUNT_FROM, for at most HUNT_TIMEOUT."
	@echo "  cert-manager                      Clone and build the pinned cert-manager controller."
	@echo "  cert-manager-crds                 Refetch the pinned cert-manager CRD release asset."
	@echo "  cert-manager-version              Print CERT_MANAGER_VERSION and nothing else."
	@echo "  verify-cert-manager-pin           Fail if the example's CRDs or the upstream tag drifted from the pin."
	@echo "  verify-cert-manager-port          Fail if port 9403, where cert-manager listens, is bound."
	@echo "  external-secrets                  Clone and build the pinned external-secrets controller."
	@echo "  external-secrets-crds             Refetch the pinned external-secrets CRD release asset."
	@echo "  external-secrets-version          Print the external-secrets version and build tags, and nothing else."
	@echo "  verify-external-secrets-pin       Fail if the example's CRDs or the upstream tag drifted from the pin."

.PHONY: setup
setup: $(SETUP_ENVTEST) vault moto
	go mod download
	@$(ENVTEST_USE) >/dev/null
	@echo "The envtest control plane for Kubernetes $(ENVTEST_K8S_VERSION) is installed."

# Silent, so that assets-path prints only the path even when this runs first.
$(SETUP_ENVTEST):
	@GOBIN=$(LOCALBIN) go install sigs.k8s.io/controller-runtime/tools/setup-envtest@$(SETUP_ENVTEST_VERSION)

.PHONY: vault
vault: $(VAULT_STAMP)

$(VAULT_STAMP):
	@mkdir -p $(LOCALBIN)
	@tmp=$$(mktemp -d) && \
	curl -fsSL -o $$tmp/vault.zip https://releases.hashicorp.com/vault/$(VAULT_VERSION)/vault_$(VAULT_VERSION)_linux_amd64.zip && \
	echo "$(VAULT_SHA256)  $$tmp/vault.zip" | sha256sum -c --quiet - && \
	unzip -oq $$tmp/vault.zip vault -d $$tmp && mv $$tmp/vault $(VAULT) && rm -rf $$tmp
	@rm -f $(LOCALBIN)/vault-*.installed && touch $@

.PHONY: moto
moto: $(MOTO_STAMP)

$(MOTO_STAMP):
	@rm -rf $(MOTO_VENV) && python3 -m venv $(MOTO_VENV)
	@$(MOTO_VENV)/bin/pip install -q --disable-pip-version-check --require-hashes -r $(MOTO_REQUIREMENTS)
	@rm -f $(LOCALBIN)/moto-*.installed && touch $@

.PHONY: assets-path
assets-path: $(SETUP_ENVTEST)
	@$(ENVTEST_USE)

.PHONY: build
build:
	go build -o bin/ ./cmd/reconciler-fuzzer ./cmd/crudfake $(TOY_PACKAGES)

.PHONY: generate $(TOYS:%=generate-%)
generate: $(TOYS:%=generate-%)

# controller-gen reads the toy's whole tree, so an API package may sit at any
# depth. It writes deepcopy code beside each API package, and CRDs to the toy's
# crds/.
$(TOYS:%=generate-%): generate-%:
	go run sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_GEN_VERSION) \
		object crd paths=./targets/$*/... output:crd:artifacts:config=targets/$*/crds

.PHONY: verify-generate
verify-generate: generate
	@stale=$$(git status --porcelain --untracked-files=all -- \
		':(glob)targets/*/crds/**' ':(glob)targets/**/zz_generated.deepcopy.go'); \
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

# go test favors the packages listed first. toy-widget's takes longest, so it
# is listed first.
.PHONY: test-generated
test-generated: setup
	KUBEBUILDER_ASSETS="$$($(ENVTEST_USE))" go test -tags envtest -count=1 -p 2 -parallel 3 -timeout 50m -run '$(GENERATED)' \
		$(filter %/toy-widget,$(TOY_PACKAGES)) $(filter-out %/toy-widget,$(TOY_PACKAGES))

# With -update, each toy's TestBugMatrixIsCurrent rewrites its own section of
# docs/bug-matrix.md. -p 1 runs the toys one at a time, since they share the file.
.PHONY: bug-matrix
bug-matrix:
	go test -count=1 -p 1 -run TestBugMatrixIsCurrent $(TOY_PACKAGES) -args -update

# Each example runs its plans and the seeds its seeds file lists, then its
# negative control. EXAMPLE_SEEDS replaces the seeds file. cert-manager also
# runs its #63 plan, which must find the bug in one of 3 attempts.
.PHONY: test-example-cert-manager
test-example-cert-manager: verify-cert-manager-pin verify-cert-manager-port setup build cert-manager
	KUBEBUILDER_ASSETS="$$($(ENVTEST_USE))" examples/test.sh cert-manager \
		plans/issue.json Clean 'secrets .*/example-tls ' --launch-arg --enable-certificate-owner-ref=false
	KUBEBUILDER_ASSETS="$$($(ENVTEST_USE))" examples/bug.sh cert-manager \
		bugs/write-back-always.json Property 'property fresh-key does not hold' 3

.PHONY: test-example-external-secrets
test-example-external-secrets: verify-external-secrets-pin setup build external-secrets
	KUBEBUILDER_ASSETS="$$($(ENVTEST_USE))" examples/test.sh external-secrets \
		controls/orphan.json Clean 'secrets .*/example-secret '
	KUBEBUILDER_ASSETS="$$($(ENVTEST_USE))" examples/test.sh external-secrets/rotation \
		controls/onchange.json Property 'property synced does not hold'

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

# A repro prints, for each plan, how many of REPRO_RUNS runs gave a finding.
# It exits 2 on a harness error.
define repro
	@for plan in $(2); do \
		found=0; \
		for i in $$(seq $(REPRO_RUNS)); do \
			out=$(HUNT_OUT)/repro-$(1)/$$(basename $$plan .json)/$$i; rm -rf $$out; \
			KUBEBUILDER_ASSETS="$$($(ENVTEST_USE))" ./bin/reconciler-fuzzer run \
				--config examples/$(1)/config.yaml --plan $$plan --out $$out >/dev/null; \
			case $$? in 0) ;; 1) found=$$((found + 1)) ;; *) cat $$out/report.md 2>/dev/null; exit 2 ;; esac; \
		done; \
		echo "$$plan: $$found of $(REPRO_RUNS) runs gave a finding."; \
	done
endef

.PHONY: repro-cert-manager
repro-cert-manager: verify-cert-manager-port setup build cert-manager
	$(call repro,cert-manager,$(or $(REPRO_PLANS),$(wildcard examples/cert-manager/bugs/*.json)))

.PHONY: repro-external-secrets
repro-external-secrets: setup build external-secrets
	$(call repro,external-secrets,$(or $(REPRO_PLANS),$(wildcard examples/external-secrets/bugs/*.json)))

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
# directives onto in-tree modules, so go install cannot reach it either. Each
# provider registers itself only under its build tag.
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
	cd $(EXTERNAL_SECRETS_SRC) && go build -tags $(EXTERNAL_SECRETS_TAGS) -o $@ .
	rm -rf $(EXTERNAL_SECRETS_SRC)

$(EXTERNAL_SECRETS_STAMP):
	rm -f $(LOCALBIN)/external-secrets-*.built
	touch $@

.PHONY: external-secrets-version
external-secrets-version:
	@echo $(EXTERNAL_SECRETS_BUILD)

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

# The ACK s3-controller example owns the lines from here to the end. It adds
# its own help:: rule.

ACK_S3_VERSION ?= v1.12.2
# The commit that tag names, and the module's go.sum hash. go install also
# checks the module against the sumdb.
ACK_S3_COMMIT ?= eace6021b60209c7d3fe216024270bf339c5e54a
ACK_S3_SUM ?= h1:tRFI61nEhMGTT6mM3PSoOKT4yC0WyzEKDO8MjbQO7wQ=
ACK_S3_MODULE := github.com/aws-controllers-k8s/s3-controller
ACK_S3 := $(LOCALBIN)/ack-s3-controller
ACK_S3_STAMP := $(LOCALBIN)/ack-s3-$(ACK_S3_VERSION).built
ACK_S3_CRDS := examples/ack-s3/crds

help::
	@echo "  test-example-ack-s3               Run the ACK s3-controller plans, seeds and negative control."
	@echo "  hunt-ack-s3                       Hunt the ACK s3-controller on HUNT_SEEDS seeds from HUNT_FROM, for at most HUNT_TIMEOUT."
	@echo "  ack-s3-controller                 Build the pinned ACK s3-controller from proxy.golang.org."
	@echo "  ack-s3-version                    Print ACK_S3_VERSION and nothing else."
	@echo "  ack-s3-crds                       Copy the pinned module's CRDs into the example and print their sha256."
	@echo "  verify-ack-s3-pin                 Fail if the example's CRDs, or the module's commit or hash, drifted from the pin."

.PHONY: test-example-ack-s3
test-example-ack-s3: verify-ack-s3-pin setup build ack-s3-controller
	KUBEBUILDER_ASSETS="$$($(ENVTEST_USE))" examples/test.sh ack-s3 \
		controls/retain.json Clean "aws buckets rf-retained "

.PHONY: hunt-ack-s3
hunt-ack-s3: verify-ack-s3-pin setup build ack-s3-controller
	$(call hunt,ack-s3)

# The nightly report's replay steps run 'make <example>'.
.PHONY: ack-s3-controller ack-s3
ack-s3-controller ack-s3: $(ACK_S3)

$(ACK_S3): $(ACK_S3_STAMP)
	@tmp=$$(mktemp -d) && \
	GOBIN=$$tmp go install $(ACK_S3_MODULE)/cmd/controller@$(ACK_S3_VERSION) && \
	mv $$tmp/controller $@ && rm -rf $$tmp

$(ACK_S3_STAMP):
	rm -f $(LOCALBIN)/ack-s3-*.built
	touch $@

.PHONY: ack-s3-version
ack-s3-version:
	@echo $(ACK_S3_VERSION)

# ack-s3-module prints one field of the downloaded module: Dir, Sum or Hash,
# its commit. It runs outside this module, so go.sum stays as it is.
ack-s3-module = $$(cd / && go mod download -json $(ACK_S3_MODULE)@$(ACK_S3_VERSION) | sed -n 's/^.*"$(1)": "\(.*\)",*$$/\1/p')

.PHONY: ack-s3-crds
ack-s3-crds:
	@dir=$(call ack-s3-module,Dir) && \
	cp $$dir/config/crd/bases/s3.services.k8s.aws_buckets.yaml $$dir/config/crd/common/bases/*.yaml $(ACK_S3_CRDS)/ && \
	chmod 644 $(ACK_S3_CRDS)/*.yaml && \
	cd $(ACK_S3_CRDS) && sha256sum *.yaml >SHA256SUMS && cat SHA256SUMS

.PHONY: verify-ack-s3-pin
verify-ack-s3-pin:
	@cd $(ACK_S3_CRDS) && sha256sum -c --quiet SHA256SUMS \
		|| { echo "$(ACK_S3_CRDS) drifted from SHA256SUMS. Run 'make ack-s3-crds' and commit the result."; exit 1; }
	@commit=$(call ack-s3-module,Hash) sum=$(call ack-s3-module,Sum); \
	if [ "$$commit" != "$(ACK_S3_COMMIT)" ] || [ "$$sum" != "$(ACK_S3_SUM)" ]; then \
		echo "$(ACK_S3_MODULE)@$(ACK_S3_VERSION) is commit $$commit with hash $$sum, and the pins are $(ACK_S3_COMMIT) and $(ACK_S3_SUM)."; \
		exit 1; \
	fi
	@dir=$(call ack-s3-module,Dir) && \
	for f in $(ACK_S3_CRDS)/*.yaml; do \
		cmp -s $$f $$dir/config/crd/bases/$$(basename $$f) || cmp -s $$f $$dir/config/crd/common/bases/$$(basename $$f) \
			|| { echo "$$f is not the $(ACK_S3_VERSION) module's file. Run 'make ack-s3-crds'."; exit 1; }; \
	done
