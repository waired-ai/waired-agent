# Top-level Makefile for waired. Today the targets focus on agent
# packaging for the GCP NAT-traversal testnet (docs/records/20260502.md);
# control / relay images are still built via scripts/infra/build-images.sh
# directly.

SHELL := /bin/bash
BUILD_SHA ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
# VERSION's fallback is a real (if obviously local) SemVer, NOT the bare
# short SHA it used to be. A build that forgets to pass VERSION should be
# recognisable as one: with the old default it was indistinguishable from
# BUILD_SHA, so the CI leg that dropped it shipped Linux binaries reporting
# `waired c0e2a1f (c0e2a1f)` for a year and nothing could tell (#631).
# `0.0.0` is also what internal/update's isDevVersion() already looks for.
# `.g<sha>` rather than `+<sha>`: the docker target below tags an image with
# this string, and `+` is not a legal character in a Docker tag.
VERSION ?= 0.0.0-dev.g$(BUILD_SHA)

# LDFLAGS_VERSION stamps the build version + commit into every binary via
# internal/buildinfo — read by `waired version`, enrollment (ClientVersion),
# the tray About dialog, and the installer-driven update check (#292).
# Appended to the -ldflags of every cmd/* build target below.
LDFLAGS_VERSION := -X github.com/waired-ai/waired-agent/internal/buildinfo.Version=$(VERSION) -X github.com/waired-ai/waired-agent/internal/buildinfo.BuildSHA=$(BUILD_SHA)

OUT_DIR     := dist
NATIVE_DIR  := $(OUT_DIR)/native
NATIVE_WIN_DIR := $(OUT_DIR)/native-windows
TARBALL     := $(OUT_DIR)/waired-agent_$(VERSION)_linux_amd64.tar.gz
TARBALL_TH  := $(OUT_DIR)/waired-agent-testharness_$(VERSION)_linux_amd64.tar.gz
TARBALL_WIN_TH := $(OUT_DIR)/waired-agent-windows-testharness_$(VERSION)_windows_amd64.tar.gz


.PHONY: help
help:
	@echo "Targets:"
	@echo "  build-agent          Build linux/amd64 waired + waired-agent into ./bin/"
	@echo "  build-agent-windows  Build windows/amd64 waired.exe + waired-agent.exe into ./bin/"
	@echo "                       (no CGO needed; produces self-contained EXEs)"
	@echo "  build-agent-darwin   Build darwin/{amd64,arm64} waired + waired-agent into ./bin/"
	@echo "                       (no CGO needed; inference engine path runs Ollama Metal"
	@echo "                       via the apple branch of engine_picker. launchd / autostart"
	@echo "                       integration is still phase W-2 follow-up.)"
	@echo "  verify-cross         go vet across linux + windows + darwin (amd64 & arm64)"
	@echo "                       — run before push to catch per-OS build breakage"
	@echo "  build-tray           Build linux/amd64 waired-tray into ./bin/"
	@echo "  build-tray-windows   Build windows/amd64 waired-tray.exe into ./bin/"
	@echo "  build-tray-darwin    Build darwin/{amd64,arm64} waired-tray into ./bin/ (CGO=1)"
	@echo "  versioninfo          Regenerate waired-tray.exe VERSIONINFO (.syso) from versioninfo.json"
	@echo "  build-agent-prod     Hardened prod agent + CLI (-tags prod; bypass flags / test routes compiled out)"
	@echo "  build-control        Build linux/amd64 waired-control into ./bin/ (rebuilds web/admin first)"
	@echo "  build-control-prod   Hardened prod waired-control (-tags prod; mock-IdP + /test/* removed)"
	@echo "  dist-agent           Produce dist/waired-agent_<sha>_linux_amd64.tar.gz"
	@echo "                       (CLI, daemon, tray, bootstrap script, systemd unit,"
	@echo "                       polkit policy, autostart .desktop)"
	@echo "  dist-agent-testharness         Linux testharness tarball (testnet VMs)"
	@echo "  dist-agent-windows-testharness Windows testharness tarball (testnet Windows VMs)"
	@echo "  dist-windows-installer End-user Windows zip + sha256 for packaging/install/install.ps1"
	@echo "                         (run on a Windows host; CI uses windows-2022)"
	@echo "  dist-darwin-installer  End-user macOS tarballs + sha256 for packaging/install/install.sh"
	@echo "                         (waired-darwin-{amd64,arm64}.tar.gz; run on a Mac / CI macos-14)"
	@echo "  docker-agent         docker build build/Dockerfile.waired-agent"
	@echo "                       (tag: waired-agent:<VERSION>)"
	@echo "  web-install          Install web/admin npm deps (npm ci)"
	@echo "  web-build            Build the management Web UI into web/admin/dist (consumed by //go:embed)"
	@echo "  web-typecheck        Run tsc --noEmit"
	@echo "  web-lint             Run eslint on the SPA"
	@echo "  web-test             Run vitest unit tests for the SPA"
	@echo "  web-check            web-typecheck + web-lint + web-test + web-build (pre-push gate)"
	@echo "  web-clean            Remove web/admin/dist (keeps .gitkeep)"
	@echo "  test-spanner         Run go test with -tags spanner (requires SPANNER_EMULATOR_HOST)"
	@echo "  smoke-control        Build + boot waired-control against Spanner Emulator + curl key endpoints"
	@echo "  e2e-inference        Real-Ollama inference e2e (gateway + router + runtime)"
	@echo "                       Pulls qwen2.5:0.5b on first run; cached after."
	@echo "  e2e-agentgrade       Grade one model's coding-agent tool-format compliance"
	@echo "                       make e2e-agentgrade MODEL=<ollama tag>  (manual; GPU)"
	@echo "                       TRIALS=<n> raises the sample; STREAM=1 drives SSE"
	@echo "                       NO_PULL=1 for a model only \`ollama create\` knows"
	@echo "  e2e-vllm             Real-vLLM inference e2e (GPU REQUIRED, ~30 min)"
	@echo "                       REQUIRED before release on any GPU host."
	@echo "                       Smoke test (~3 min): make e2e-vllm-quick"
	@echo "  e2e-vllm-quick       Real-vLLM smoke only (Qwen2.5-0.5B, GPU REQUIRED)"
	@echo "  e2e-vllm-fp8         fp8 KV cache ≈2× pool on Ada+ (GPU REQUIRED, #676)"
	@echo "  e2e-vllm-spec        ngram speculative decode boots+serves (GPU REQUIRED, #677)"
	@echo "  e2e-vllm-power       engine stop really frees VRAM; a crash is noticed; the"
	@echo "                       engine outlives its caller (GPU REQUIRED, #881/#946/#947)"
	@echo "  integration-runtime  Real-Ollama lifecycle test (no model pull)"
	@echo "  catalog-docs         Regenerate the model table (docs/reference/models.md) from proto/catalog/bundled"
	@echo "  verify-catalog-docs  Fail if that model table drifted from the bundled catalog"
	@echo ""
	@echo "End-user packaging (.deb via nfpm; requires nfpm in PATH):"
	@echo "  deb-all              Build waired + waired-tray .deb for amd64 and arm64"
	@echo "  deb-amd64 deb-arm64  Same, for one arch"
	@echo "  ci-lint-local        run the scripts/ci guards ci.yml's lint job runs"
	@echo "  install-script-lint  shellcheck install.sh + .deb maintainer + CI scripts"
	@echo "  ps-script-lint       PSScriptAnalyzer over the shipped .ps1 scripts"

.PHONY: build-agent
build-agent:
	@mkdir -p bin
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
	  go build -trimpath -ldflags="-s -w $(LDFLAGS_VERSION)" -o bin/waired ./cmd/waired
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
	  go build -trimpath -ldflags="-s -w $(LDFLAGS_VERSION)" -o bin/waired-agent ./cmd/waired-agent

# build-agent-prod builds the HARDENED production agent + CLI (-tags prod).
# The dev/e2e bypass flags (--bypass-idp / --cookies-insecure /
# --enable-oidc-grant / --bypass-cp-iam / --bypass-mode / --bypass-email) and
# the mock-IdP /test/* routes are compiled out via internal/buildflag, so they
# cannot be re-enabled at runtime (docs/specs/environments-and-release.md §3.1,
# §6.4). Outputs are suffixed -prod so they never clobber the dev binaries.
.PHONY: build-agent-prod
build-agent-prod:
	@mkdir -p bin
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
	  go build -trimpath -tags prod -ldflags="-s -w $(LDFLAGS_VERSION)" -o bin/waired-prod ./cmd/waired
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
	  go build -trimpath -tags prod -ldflags="-s -w $(LDFLAGS_VERSION)" -o bin/waired-agent-prod ./cmd/waired-agent

# build-agent-windows cross-compiles the Windows EXEs from any host.
# CGO_ENABLED=0 keeps the build pure Go (no C toolchain needed); the
# resulting binaries embed Go's net/HTTP/TLS stack and Win32 syscalls
# from golang.org/x/sys/windows. Outputs land at bin/waired.exe and
# bin/waired-agent.exe — copy them to the target Windows box and run
# `waired-agent.exe install` as Administrator to register the service.
.PHONY: build-agent-windows
build-agent-windows:
	@mkdir -p bin
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 \
	  go build -trimpath -ldflags="-s -w $(LDFLAGS_VERSION)" -o bin/waired.exe ./cmd/waired
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 \
	  go build -trimpath -ldflags="-s -w $(LDFLAGS_VERSION)" -o bin/waired-agent.exe ./cmd/waired-agent

# build-agent-darwin builds both Intel and Apple Silicon variants. The
# inference engine path is wired through to Ollama Metal via the
# `apple` branch of internal/router.PickEngine (vLLM is explicitly
# unsupported on darwin per internal/runtime/vllm_stub_darwin.go).
# launchd-driven service install / autostart on macOS is implemented
# (internal/platform/service/service_darwin.go writes a per-user
# LaunchAgent), so `waired-agent install` registers the daemon with
# launchd. The binary is unsigned (ad-hoc); signing/notarization is a
# follow-up (#262).
.PHONY: build-agent-darwin
build-agent-darwin:
	@mkdir -p bin
	CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 \
	  go build -trimpath -ldflags="-s -w $(LDFLAGS_VERSION)" -o bin/waired-darwin-amd64 ./cmd/waired
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 \
	  go build -trimpath -ldflags="-s -w $(LDFLAGS_VERSION)" -o bin/waired-darwin-arm64 ./cmd/waired
	CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 \
	  go build -trimpath -ldflags="-s -w $(LDFLAGS_VERSION)" -o bin/waired-agent-darwin-amd64 ./cmd/waired-agent
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 \
	  go build -trimpath -ldflags="-s -w $(LDFLAGS_VERSION)" -o bin/waired-agent-darwin-arm64 ./cmd/waired-agent

# verify-cross runs `go vet` across every supported OS / arch combo.
# Cheap (<10s on a warm cache) and catches breakage of the form
# "I added a Linux-only function and forgot the _linux.go suffix"
# before push.
#
# Darwin tray exclusion: fyne.io/systray's darwin backend (NSStatusItem
# via Cocoa) requires CGO_ENABLED=1, but cross-vet runs CGO_ENABLED=0
# by default to keep the matrix portable from a Linux CI runner.
# Vetting cmd/waired-tray + internal/gui/tray under GOOS=darwin with
# CGO disabled fails because the systray native symbols are hidden
# behind `import "C"`. We skip those three packages on darwin and
# rely on `make build-tray-darwin` (CGO=1) — run natively on a Mac —
# as the build-tag gate for that subtree. Linux/Windows already vet
# the full set; their systray backends are pure Go.
#
# That native leg is not "someone's laptop, eventually": the paths-gated
# tray-darwin.yml job runs vet, the unit suite, and this target on a
# macos-14 runner for every PR touching the subtree (waired#901 I1, with
# the unit suite added by #152), so a darwin-only break fails the PR
# instead of turning main red after the merge.
# scripts/ci/tray-darwin-paths-guard.sh keeps that filter — and this
# exclusion's rationale — from drifting apart.
DARWIN_VET_PKGS = $(shell go list ./... | grep -v -E '(cmd/waired-tray|internal/gui/tray)')

.PHONY: verify-cross
verify-cross:
	GOOS=linux   GOARCH=amd64 go vet ./...
	GOOS=windows GOARCH=amd64 go vet ./...
	GOOS=darwin  GOARCH=amd64 go vet $(DARWIN_VET_PKGS)
	GOOS=darwin  GOARCH=arm64 go vet $(DARWIN_VET_PKGS)

# catalog-docs regenerates the machine-generated model table
# (docs/reference/models.md) from proto/catalog/bundled/*.json.
# Run after adding or editing a bundled manifest; catalog-radar runs the same
# step inside its draft PRs.
.PHONY: catalog-docs
catalog-docs:
	go run ./cmd/catalog-tool docs

# verify-catalog-docs fails if the committed model table drifted from the bundled
# catalog (mirrors `catalog-tool validate --all`). The unit test
# TestModelCatalogPageFresh enforces the same invariant in the `unit` CI job.
.PHONY: verify-catalog-docs
verify-catalog-docs:
	go run ./cmd/catalog-tool docs --check

# versioninfo regenerates the Windows VERSIONINFO resource
# (cmd/waired-tray/resource_windows_amd64.syso) from versioninfo.json so
# waired-tray.exe reports the user-facing name "Waired" in Task Manager /
# Explorer (waired#810). The .syso is committed and auto-linked on
# windows/amd64 only; run this only after editing versioninfo.json.
.PHONY: versioninfo
versioninfo:
	go generate ./cmd/waired-tray

# build-tray builds the desktop tray binary. CGO is left at default —
# fyne.io/systray's Linux backend talks DBus via pure-Go godbus and
# does not need a C toolchain. Version + buildSHA are stamped via
# -ldflags so `About Waired` shows accurate values.
.PHONY: build-tray
build-tray:
	@mkdir -p bin
	GOOS=linux GOARCH=amd64 \
	  go build -trimpath \
	    -ldflags="-s -w $(LDFLAGS_VERSION)" \
	    -o bin/waired-tray ./cmd/waired-tray

# build-tray-windows cross-compiles the Windows tray binary.
# -H windowsgui strips the otherwise-spawned console window so
# double-clicking the .exe gives the user a clean tray-only UX —
# this is the linker subsystem flag, identical to MSVC's
# /SUBSYSTEM:WINDOWS. CGO is disabled because fyne.io/systray's
# Windows backend uses pure-Go syscalls into user32/shell32 only.
.PHONY: build-tray-windows
build-tray-windows:
	@mkdir -p bin
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 \
	  go build -trimpath \
	    -ldflags="-s -w -H windowsgui $(LDFLAGS_VERSION)" \
	    -o bin/waired-tray-windows-amd64.exe ./cmd/waired-tray

# build-tray-darwin builds the macOS tray binary. CGO is mandatory
# (CGO_ENABLED=1): fyne.io/systray's darwin backend uses NSStatusItem
# via Objective-C runtime, which means each TU touches Cocoa headers.
# The default macOS toolchain (clang shipped by Xcode CLT) handles
# both arm64 (Apple Silicon, native) and amd64 (Intel, requires an
# x86_64 SDK present on the build host — `xcode-select --install`
# brings it down). The "ad-hoc" binary is unsigned; LaunchAgent
# bootstrap from internal/platform/service/service_darwin.go is what
# wires it to start at login, and the installer phase (TBD) will add
# code signing + notarization.
.PHONY: build-tray-darwin
build-tray-darwin:
	@mkdir -p bin
	CGO_ENABLED=1 GOOS=darwin GOARCH=arm64 \
	  go build -trimpath \
	    -ldflags="-s -w $(LDFLAGS_VERSION)" \
	    -o bin/waired-tray-darwin-arm64 ./cmd/waired-tray
	CGO_ENABLED=1 GOOS=darwin GOARCH=amd64 \
	  go build -trimpath \
	    -ldflags="-s -w $(LDFLAGS_VERSION)" \
	    -o bin/waired-tray-darwin-amd64 ./cmd/waired-tray

.PHONY: dist-agent
dist-agent: build-agent build-tray
	@rm -rf $(NATIVE_DIR)
	@mkdir -p $(NATIVE_DIR)/bin $(NATIVE_DIR)/systemd $(NATIVE_DIR)/autostart $(NATIVE_DIR)/applications $(NATIVE_DIR)/polkit
	cp bin/waired bin/waired-agent bin/waired-tray $(NATIVE_DIR)/bin/
	cp build/agent-bootstrap.sh $(NATIVE_DIR)/bin/
	cp build/install-desktop.sh $(NATIVE_DIR)/bin/
	cp build/waired-agent.service $(NATIVE_DIR)/systemd/
	cp build/autostart/waired-tray.desktop $(NATIVE_DIR)/autostart/
	cp build/applications/waired-tray.desktop $(NATIVE_DIR)/applications/
	cp build/polkit/com.waired.policy $(NATIVE_DIR)/polkit/
	chmod +x $(NATIVE_DIR)/bin/agent-bootstrap.sh
	chmod +x $(NATIVE_DIR)/bin/install-desktop.sh
	tar czf $(TARBALL) -C $(NATIVE_DIR) .
	@echo "==> wrote $(TARBALL)"
	@ls -la $(TARBALL)

# build-agent-testharness produces a waired-agent binary compiled
# with the `testharness` build tag — the iptables-driven scenario
# dispatcher used by the testnet fallback gate. Output is at
# bin/waired-agent-testharness so it can co-exist with the production
# bin/waired-agent from `build-agent`.
.PHONY: build-agent-testharness
build-agent-testharness:
	@mkdir -p bin
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
	  go build -trimpath -ldflags="-s -w" -tags testharness \
	    -o bin/waired-agent-testharness ./cmd/waired-agent

# dist-agent-testharness produces the AR generic tarball consumed by
# testnet VMs' startup-native.sh.tpl. Inside the tarball:
#   bin/waired                  — production CLI (used by
#                                 agent-bootstrap.sh's init step)
#   bin/waired-agent            — the *testharness* binary (renamed
#                                 from waired-agent-testharness so the
#                                 systemd ExecStart path is identical
#                                 to the production layout)
#   systemd/waired-agent.service — the *testharness* unit (User=root +
#                                  CAP_NET_ADMIN/NET_RAW), again
#                                  installed under the production unit
#                                  name so startup-native.sh.tpl needs
#                                  no template-time switch.
# Headless: tray, autostart .desktop, polkit policy, install-desktop.sh
# are deliberately omitted from this tarball — testnet VMs are
# noninteractive.
.PHONY: dist-agent-testharness
dist-agent-testharness: build-agent build-agent-testharness
	@rm -rf $(NATIVE_DIR)
	@mkdir -p $(NATIVE_DIR)/bin $(NATIVE_DIR)/systemd
	cp bin/waired $(NATIVE_DIR)/bin/
	cp bin/waired-agent-testharness $(NATIVE_DIR)/bin/waired-agent
	cp build/agent-bootstrap.sh $(NATIVE_DIR)/bin/
	cp build/waired-agent-testharness.service $(NATIVE_DIR)/systemd/waired-agent.service
	chmod +x $(NATIVE_DIR)/bin/agent-bootstrap.sh
	tar czf $(TARBALL_TH) -C $(NATIVE_DIR) .
	@echo "==> wrote $(TARBALL_TH)"
	@ls -la $(TARBALL_TH)

# build-agent-windows-testharness cross-compiles a Windows agent with
# the testharness build tag. Used by the Windows companion VMs in the
# testnet (Phase W-2 / modules/test-agents-windows). On Windows the
# testharness scenarios package compiles to a non-iptables stub
# (internal/testharness/scenarios/registry_other.go) so the agent
# becomes a scenario-observer rather than a scenario-injector.
.PHONY: build-agent-windows-testharness
build-agent-windows-testharness:
	@mkdir -p bin
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 \
	  go build -trimpath -ldflags="-s -w" \
	    -o bin/waired-windows-amd64.exe ./cmd/waired
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 \
	  go build -trimpath -ldflags="-s -w" -tags testharness \
	    -o bin/waired-agent-windows-testharness.exe ./cmd/waired-agent

# dist-agent-windows-testharness produces the AR generic tarball that
# infra/terraform/modules/test-agents-windows/startup.ps1.tpl pulls down
# on GCE Windows VM bootstrap. Layout:
#   bin/waired.exe              — CLI (used by `waired init --bypass-mode`)
#   bin/waired-agent.exe        — *testharness* daemon (renamed so the
#                                 startup script's install command path
#                                 matches the production layout)
# tar+gzip rather than zip so the same archive format works on both
# sides; PowerShell 5.1+ ships `tar` (bsdtar) by default on Windows
# Server 2022 / Windows 11.
.PHONY: dist-agent-windows-testharness
dist-agent-windows-testharness: build-agent-windows-testharness
	@rm -rf $(NATIVE_WIN_DIR)
	@mkdir -p $(NATIVE_WIN_DIR)/bin
	cp bin/waired-windows-amd64.exe $(NATIVE_WIN_DIR)/bin/waired.exe
	cp bin/waired-agent-windows-testharness.exe $(NATIVE_WIN_DIR)/bin/waired-agent.exe
	tar czf $(TARBALL_WIN_TH) -C $(NATIVE_WIN_DIR) .
	@echo "==> wrote $(TARBALL_WIN_TH)"
	@ls -la $(TARBALL_WIN_TH)

# dist-windows-installer is the end-user-facing Windows artifact:
#   dist/waired-windows-amd64.zip          # waired.exe + waired-agent.exe + waired-tray.exe + VERSION
#   dist/waired-windows-amd64.zip.sha256   # consumed by packaging/install/install.ps1 verification
# Intended to run on a Windows host (CI: windows-2022; dev: any Windows
# with Go + Git Bash). Linux/macOS hosts can produce the EXEs via
# build-agent-windows / build-tray-windows but cannot pack the zip from
# this target — Compress-Archive is invoked via powershell.exe.
WIN_DIST_DIR := $(OUT_DIR)/windows-amd64
.PHONY: dist-windows-installer
dist-windows-installer: build-agent-windows build-tray-windows
	@rm -rf $(WIN_DIST_DIR)
	@mkdir -p $(WIN_DIST_DIR)
	@test -f $(OUT_DIR)/THIRD_PARTY_LICENSES -a -f $(OUT_DIR)/LICENSE || { \
	  echo "==> $(OUT_DIR)/{LICENSE,THIRD_PARTY_LICENSES} missing — generating (go-licenses; needs network on first run)"; \
	  $(MAKE) third-party-licenses; }
	cp bin/waired.exe                    $(WIN_DIST_DIR)/waired.exe
	cp bin/waired-agent.exe              $(WIN_DIST_DIR)/waired-agent.exe
	cp bin/waired-tray-windows-amd64.exe $(WIN_DIST_DIR)/waired-tray.exe
	cp $(OUT_DIR)/LICENSE                 $(WIN_DIST_DIR)/LICENSE
	cp $(OUT_DIR)/THIRD_PARTY_LICENSES    $(WIN_DIST_DIR)/THIRD_PARTY_LICENSES
	echo $(VERSION) > $(WIN_DIST_DIR)/VERSION
	powershell.exe -NoProfile -ExecutionPolicy Bypass \
	    -File packaging/windows/make-zip.ps1 \
	    -SourceDir $(WIN_DIST_DIR) \
	    -OutZip $(OUT_DIR)/waired-windows-amd64.zip
	@echo "==> wrote $(OUT_DIR)/waired-windows-amd64.zip"
	@ls -la $(OUT_DIR)/waired-windows-amd64.zip $(OUT_DIR)/waired-windows-amd64.zip.sha256

# dist-darwin-installer is the end-user-facing macOS artifact, the
# darwin analogue of dist-windows-installer. For each arch it emits:
#   dist/waired-darwin-<arch>.tar.gz          # waired + waired-agent + waired-tray + VERSION
#   dist/waired-darwin-<arch>.tar.gz.sha256   # hex digest, verified by install.sh
# The tray (waired-tray) is now bundled, matching the Windows zip and
# Linux .deb which already ship it. The darwin tray needs CGO=1 + the
# Xcode CLT, so this target depends on build-tray-darwin and must run on
# a Mac (CI: macos-14) — unlike build-agent-darwin (CGO_ENABLED=0), which
# is cross-compilable from any host. The tray binary is unsigned ad-hoc;
# .app/.dmg + Homebrew + notarization remain deferred under #262.
# `shasum` is assumed present (true on macOS; coreutils provides it on
# Linux).
DARWIN_DIST_DIR := $(OUT_DIR)/darwin
DARWIN_ARCHES   := amd64 arm64
.PHONY: dist-darwin-installer
dist-darwin-installer: build-agent-darwin build-tray-darwin
	@test -f $(OUT_DIR)/THIRD_PARTY_LICENSES -a -f $(OUT_DIR)/LICENSE || { \
	  echo "==> $(OUT_DIR)/{LICENSE,THIRD_PARTY_LICENSES} missing — generating (go-licenses; needs network on first run)"; \
	  $(MAKE) third-party-licenses; }
	@for arch in $(DARWIN_ARCHES); do \
	  d=$(DARWIN_DIST_DIR)/$$arch; \
	  rm -rf $$d; mkdir -p $$d; \
	  cp bin/waired-darwin-$$arch       $$d/waired; \
	  cp bin/waired-agent-darwin-$$arch $$d/waired-agent; \
	  cp bin/waired-tray-darwin-$$arch  $$d/waired-tray; \
	  cp $(OUT_DIR)/LICENSE              $$d/LICENSE; \
	  cp $(OUT_DIR)/THIRD_PARTY_LICENSES $$d/THIRD_PARTY_LICENSES; \
	  echo $(VERSION) > $$d/VERSION; \
	  tar czf $(OUT_DIR)/waired-darwin-$$arch.tar.gz -C $$d . ; \
	  shasum -a 256 $(OUT_DIR)/waired-darwin-$$arch.tar.gz | awk '{print $$1}' \
	    > $(OUT_DIR)/waired-darwin-$$arch.tar.gz.sha256; \
	  echo "==> wrote $(OUT_DIR)/waired-darwin-$$arch.tar.gz"; \
	done
	@ls -la $(OUT_DIR)/waired-darwin-*.tar.gz $(OUT_DIR)/waired-darwin-*.tar.gz.sha256

.PHONY: docker-agent
docker-agent:
	docker build -f build/Dockerfile.waired-agent -t waired-agent:$(VERSION) .

# Inference e2e: spin up a real ollama subprocess on a free port,
# pull the smallest practical model (qwen2.5:0.5b, ~400MB cached after
# the first run), bring up the production gateway/router stack, and
# exercise both the OpenAI and Anthropic compat surfaces. Requires a
# locally installed `ollama` binary; skips otherwise.
.PHONY: e2e-inference
e2e-inference:
	go test -tags e2e -count=1 -v -timeout=15m -run TestInferenceGatewayE2E ./internal/e2e/inference/...

# Agent-harness grade (#322): drive the coding-agent-shaped fixture at the
# real gateway against one model and report whether it can emit structured
# tool calls under that weight. This is the measurement behind the catalog's
# agent-grade verdicts.
#
# Manual on purpose — never per-PR, never nightly. It needs a real model
# resident and a GPU to be worth anything, and a scheduled run would let a
# registry hiccup record itself as a model demotion (waired-ai/waired-agent#203
# is the same mistake in the benchmark path). The verdict is a judgement about
# a model, so a human asks for it.
#
#   make e2e-agentgrade MODEL=qwen3.6:27b-q4_K_M
#   make e2e-agentgrade MODEL=<tag> JSON=/tmp/verdict.json
#   make e2e-agentgrade MODEL=<tag> TRIALS=12   # re-measure a 1/3 blip
#   make e2e-agentgrade MODEL=<tag> STREAM=1    # the path a coding agent takes
#   make e2e-agentgrade MODEL=<tag> NO_PULL=1   # a locally derived model, not in the registry
#
# The target — not a bare `go test` — is what stamps the commit the probe ran
# from into the report. FixtureRevision covers the fixture and nothing else, so
# without this a verdict cannot say WHICH gateway produced it; #409 changed the
# answer for four models without moving the fixture by a byte. `catalog-tool
# agentgrade --import` refuses a report that is unstamped or built from a dirty
# tree, which is why measuring is a make target and not a habit.
#
# "Dirty" means a change to the code under measurement: any modification to a
# TRACKED file, or an untracked *.go (which compiles into its package all the
# same). Other untracked files are excluded deliberately — the CI grade job runs
# `make e2e-agentgrade ... | tee agentgrade.log`, and the shell creates that log
# in the repo root before make starts, so a strict check marked every dispatched
# measurement `-dirty` and made its own report unimportable.
# The run also drives the request-shape matrix (waired-agent#1095) and folds
# it into the same report: about six requests at max_tokens 1, engine-direct,
# asking whether this model's chat template renders the shapes a coding agent
# sends. Fold it in with `catalog-tool shapes --import` on the same file.
.PHONY: e2e-agentgrade
e2e-agentgrade:
	@test -n "$(MODEL)" || { echo "usage: make e2e-agentgrade MODEL=<ollama tag> [JSON=<path>]"; exit 2; }
	@rev="$$(git rev-parse --short=12 HEAD 2>/dev/null)"; \
	 test -n "$$rev" || { echo "e2e-agentgrade: not a git checkout — cannot stamp the probe revision"; exit 2; }; \
	 if [ -n "$$(git status --porcelain --untracked-files=no 2>/dev/null)" ] \
	    || [ -n "$$(git ls-files --others --exclude-standard -- '*.go' 2>/dev/null)" ]; then \
	   rev="$$rev-dirty"; \
	 fi; \
	 echo "probe revision: $$rev"; \
	 WAIRED_AGENTGRADE_MODEL="$(MODEL)" WAIRED_AGENTGRADE_JSON="$(JSON)" \
	 WAIRED_AGENTGRADE_TRIALS="$(TRIALS)" WAIRED_AGENTGRADE_STREAM="$(STREAM)" \
	 WAIRED_AGENTGRADE_NO_PULL="$(NO_PULL)" \
	   go test -tags e2e -count=1 -v -timeout=40m \
	     -ldflags "-X github.com/waired-ai/waired-agent/internal/agentgrade.agentRevision=$$rev" \
	     -run TestAgentGrade ./internal/e2e/agentgrade/...

# vLLM e2e (GPU REQUIRED): exercises the Step-2 multi-engine path —
# venv install (uv-managed Python 3.12), HF download (huggingface-cli +
# hf_transfer), VLLMAdapter spawn against a real GPU, and the full
# /v1/chat/completions surface served from vllm. The smoke test uses
# Qwen2.5-0.5B (~1 GB download, ~3 min total); the realistic pass
# uses Qwen3-14B-Instruct-AWQ (~9 GB, ~30 min).
#
# REQUIRED before any release made from a GPU-equipped host. CI must
# include a GPU lane that runs this target — see waired/docs/decisions/
# 'GPU test mandate' entry.
.PHONY: e2e-vllm
e2e-vllm:
	go test -tags=e2e,gpu -count=1 -v -timeout=45m -run TestVLLMGatewayE2E ./internal/e2e/inference/...

.PHONY: e2e-vllm-quick
e2e-vllm-quick:
	go test -tags=e2e,gpu -count=1 -v -short -timeout=15m -run TestVLLMGatewayE2E ./internal/e2e/inference/...

# #675 max-model-len clamp lanes: proves an unfittable window aborts
# startup and the router.VLLMMaxModelLen estimate boots and serves on
# the same (model, GPU, utilization) tuple. Uses the AWQ realistic
# model (~9 GB download).
.PHONY: e2e-vllm-clamp
e2e-vllm-clamp:
	go test -tags=e2e,gpu -count=1 -v -timeout=45m -run TestVLLMMaxModelLenClamp ./internal/e2e/inference/...

# #676 fp8 KV cache: on an Ada+ host, proves fp8 (e4m3) roughly doubles
# the engine-reported KV pool vs fp16 at the same weights/util/window and
# still boots + serves. Uses the AWQ realistic model (~9 GB download).
.PHONY: e2e-vllm-fp8
e2e-vllm-fp8:
	go test -tags=e2e,gpu -count=1 -v -timeout=45m -run TestVLLMFP8KVCache ./internal/e2e/inference/...

# #677 ngram speculative decoding: proves ngram boots + serves a
# coding-style generation and logs the single-stream decode tok/s with
# and without speculation for the default decision.
.PHONY: e2e-vllm-spec
e2e-vllm-spec:
	go test -tags=e2e,gpu -count=1 -v -timeout=45m -run TestVLLMSpeculativeNgram ./internal/e2e/inference/...

# #881/#946/#947 engine power and engine lifetime: proves the VRAM actually
# comes back on a stop, that a stopped engine stays stopped, that a crash is
# noticed, and that the engine outlives the caller that started it without
# orphaning a worker to init. One target for all three because they are one
# surface, and because every extra target is another chance for one to end up
# with no caller — which is what waired#1229 was.
.PHONY: e2e-vllm-power
e2e-vllm-power:
	go test -tags=e2e,gpu -count=1 -v -timeout=45m -run TestVLLMEnginePower ./internal/e2e/inference/...

# Run only the (fast) ollama-runtime integration test.
.PHONY: integration-runtime
integration-runtime:
	go test -tags integration -count=1 -v ./internal/runtime/...

# ---------------------------------------------------------------------
# End-user .deb packaging via nfpm.
#
# These targets are independent of `dist-agent` / `dist-agent-testharness`
# (which produce GCE / testnet tarballs). Output lives at
# dist/waired_*_<arch>.deb and dist/waired-tray_*_<arch>.deb.
#
# Requires nfpm in PATH (https://nfpm.goreleaser.com/). PKG_VERSION
# is normalised so the Debian version comparator works regardless of
# whether it is a tag or a local dev string.
# ---------------------------------------------------------------------

GOARCHES_DEB ?= amd64 arm64

# Local-dev fallback only: a leading digit for Debian's upstream_version
# rules, and `~` so it sorts below every real release — the same rewrite CI
# applies to the edge version. Derived from BUILD_SHA rather than VERSION so
# it does not double up VERSION's own `0.0.0-dev.` prefix.
#
# CI passes PKG_VERSION explicitly — the tag (v stripped) for releases, or
# <core>~edge.<ts>+<sha> for edge — from the `version` job in
# .github/workflows/reusable-build-artifacts.yml (nfpm version_schema is
# 'none', so the value is used verbatim). That job also passes VERSION, which
# is a DIFFERENT string and stamps a different thing: PKG_VERSION is the
# package's Version field, VERSION goes into the binary via LDFLAGS_VERSION.
# Passing only one of them is #631. Override locally with
# `make deb-all PKG_VERSION=1.2.3`.
PKG_VERSION ?= 0.0.0~dev.g$(BUILD_SHA)

.PHONY: build-linux-multiarch
build-linux-multiarch: $(addprefix build-linux-,$(GOARCHES_DEB))

build-linux-%:
	@mkdir -p bin/linux_$*
	CGO_ENABLED=0 GOOS=linux GOARCH=$* \
	  go build -trimpath -ldflags="-s -w $(LDFLAGS_VERSION)" \
	    -o bin/linux_$*/waired ./cmd/waired
	CGO_ENABLED=0 GOOS=linux GOARCH=$* \
	  go build -trimpath -ldflags="-s -w $(LDFLAGS_VERSION)" \
	    -o bin/linux_$*/waired-agent ./cmd/waired-agent
	GOOS=linux GOARCH=$* \
	  go build -trimpath \
	    -ldflags="-s -w $(LDFLAGS_VERSION)" \
	    -o bin/linux_$*/waired-tray ./cmd/waired-tray

# License notices staged into dist/ for the .deb contents (#666): the
# nfpm templates bundle dist/LICENSE + dist/THIRD_PARTY_LICENSES into
# /usr/share/doc/<pkg>/. CI runs this before `make deb-*`; locally it
# needs network access on first run (go run fetches go-licenses).
.PHONY: third-party-licenses
third-party-licenses:
	bash scripts/ci/gen-third-party-licenses.sh

.PHONY: deb-all
deb-all: $(addprefix deb-,$(GOARCHES_DEB))

deb-%: build-linux-%
	@command -v nfpm >/dev/null 2>&1 || { \
	  echo "error: nfpm not found in PATH (https://nfpm.goreleaser.com/)" >&2; \
	  exit 1; }
	@test -f $(OUT_DIR)/THIRD_PARTY_LICENSES -a -f $(OUT_DIR)/LICENSE || { \
	  echo "==> $(OUT_DIR)/{LICENSE,THIRD_PARTY_LICENSES} missing — generating (go-licenses; needs network on first run)"; \
	  $(MAKE) third-party-licenses; }
	@command -v envsubst >/dev/null 2>&1 || { \
	  echo "error: envsubst not found (apt install gettext-base)" >&2; exit 1; }
	@mkdir -p $(OUT_DIR) $(OUT_DIR)/nfpm
	ARCH=$* PKG_VERSION=$(PKG_VERSION) \
	  envsubst '$$ARCH $$PKG_VERSION' \
	    < packaging/nfpm/waired.yaml.tmpl \
	    > $(OUT_DIR)/nfpm/waired-$*.yaml
	ARCH=$* PKG_VERSION=$(PKG_VERSION) \
	  envsubst '$$ARCH $$PKG_VERSION' \
	    < packaging/nfpm/waired-tray.yaml.tmpl \
	    > $(OUT_DIR)/nfpm/waired-tray-$*.yaml
	nfpm pkg --config $(OUT_DIR)/nfpm/waired-$*.yaml \
	  --packager deb \
	  --target $(OUT_DIR)/waired_$(PKG_VERSION)_$*.deb
	nfpm pkg --config $(OUT_DIR)/nfpm/waired-tray-$*.yaml \
	  --packager deb \
	  --target $(OUT_DIR)/waired-tray_$(PKG_VERSION)_$*.deb
	@echo "==> built $* debs:"
	@ls -la $(OUT_DIR)/waired_$(PKG_VERSION)_$*.deb $(OUT_DIR)/waired-tray_$(PKG_VERSION)_$*.deb

# The target list deliberately lives in scripts/ci/install-script-lint.sh,
# not here: this Makefile is listed whole in
# scripts/ci/testnet-relevant-paths.txt (it builds the shipped binaries and
# the testnet harness inputs) and the gate matches by plain path prefix, so
# it cannot see which target a diff touched. Adding one script name to the
# lint list here armed a ~25-minute real-NAT run on PRs that ship nothing
# (#292). Add lint targets to that script; leave this entry point alone.
.PHONY: install-script-lint
install-script-lint:
	bash scripts/ci/install-script-lint.sh

# The guards ci.yml's lint job runs that nothing local ran before (#749).
# Same rule as the entry point above: the list lives in the script, derived
# from the workflow, so a new guard needs no edit here and no edit there.
.PHONY: ci-lint-local
ci-lint-local:
	bash scripts/dev/ci-lint-local.sh

# The PowerShell half of install-script-lint. Separate because the two have
# different prerequisites -- shellcheck is a package, PSScriptAnalyzer is a
# PSGallery module inside pwsh -- and a dev box that has one rarely has both.
# Rules that do not apply to an installer are declared, with reasons, in
# scripts/ci/PSScriptAnalyzerSettings.psd1.
.PHONY: ps-script-lint
ps-script-lint:
	@command -v pwsh >/dev/null 2>&1 || { \
	  echo "error: pwsh not found in PATH" >&2; exit 1; }
	pwsh -NoProfile -File scripts/ci/ps-script-lint.ps1
