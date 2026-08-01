package main

// declared is the single source of truth for exec.LookPath call sites in
// cmd/ and internal/. Nothing else clears the guard, so every place this
// repository asks "$PATH, is X there?" is readable in one list — which is
// exactly what was missing: five mutually contradicting engine predicates
// grew because each one looked reasonable in its own file (#179).
//
// Reason says why PATH is the right question at that site. "A system or
// third-party tool the host either has or does not" is a good reason.
// "A waired-managed binary" is not: those resolve through
// cmd/waired-agent/engine_resolve.go, which stats the state dir, because
// waired's own engine is deliberately off $PATH and a Windows
// LocalSystem service inherits no user PATH at all.
type lookpath struct {
	File   string
	Binary string
	Reason string
}

const (
	privHelper = "privilege-escalation helper; a host either has it on PATH or does not, " +
		"and choosing between them is the point of the probe"
	desktopHelper = "desktop helper binary; picking whichever the user's desktop ships " +
		"is precisely a PATH question"
	systemTool = "OS-provided system tool, not a waired component"
	vendorTool = "GPU vendor driver tool, probed as ONE step of a chain that continues " +
		"past a PATH miss — never the verdict on its own"
	userTool = "third-party CLI the user installed themselves; waired never manages it"
	ownCLI   = "waired's own CLI, which the installer puts on PATH because it is what " +
		"the user types; a state-dir lookup would not find it"
)

var declared = []lookpath{
	// waired's own CLI. The installer puts it on PATH on purpose — unlike
	// the ENGINE, this really is a PATH question. Every site here falls
	// back gracefully when it is absent.
	{"cmd/waired-agent/binary_path.go", "waired", ownCLI},
	{"internal/gui/tray/actions_darwin.go", "waired", ownCLI},
	{"internal/gui/tray/actions_linux.go", "waired", ownCLI},
	{"internal/gui/tray/actions_windows.go", "waired.exe", ownCLI},

	// Privilege-escalation helpers. Probing them is how the fallback
	// chain picks one.
	{"cmd/waired/codeui.go", "runuser", privHelper},
	{"cmd/waired/codeui.go", "sudo", privHelper},
	{"cmd/waired/init_integration.go", "runuser", privHelper},
	{"cmd/waired/init_integration.go", "sudo", privHelper},
	{"internal/platform/browser/browser_linux.go", "runuser", privHelper},
	{"internal/platform/browser/browser_linux.go", "sudo", privHelper},
	{"internal/gui/tray/actions_linux.go", "pkexec", privHelper},
	// The tray-host repair (#295) needs both directions: sudo to raise apt to
	// root, runuser/sudo to drop back to the desktop user whose dconf holds
	// the enabled-extensions list.
	{"internal/platform/trayhost/repair_linux.go", "runuser", privHelper},
	{"internal/platform/trayhost/repair_linux.go", "sudo", privHelper},

	// Desktop helpers. `prog.binary` walks the zenity/kdialog candidate list.
	{"internal/gui/tray/actions_linux.go", "zenity", desktopHelper},
	{"internal/gui/tray/actions_linux.go", "kdialog", desktopHelper},
	{"internal/gui/tray/dialog_linux.go", "prog.binary", desktopHelper},
	{"internal/platform/notification/notification_linux.go", "notify-send", desktopHelper},
	// Tray-host repair (#295). gnome-shell is the load-bearing one: it is what
	// tells a GNOME desktop apart from a server, and PlanRepair refuses to
	// plan an apt install without it — on Ubuntu 26.04 the extension name is a
	// virtual package provided by gnome-shell-ubuntu-extensions, which
	// `Depends: gnome-shell`, so installing it on a host that has none would
	// pull a whole desktop onto a server.
	{"internal/platform/trayhost/repair_linux.go", "gnome-shell", desktopHelper},
	{"internal/platform/trayhost/repair_linux.go", "gnome-extensions", desktopHelper},

	// OS-provided system tools.
	{"internal/platform/service/service_linux.go", "systemctl", systemTool},
	// The tray's daemon-down "Start the Waired agent" row runs
	// `pkexec <systemctl> start waired-agent`; pkexec needs the absolute path
	// because polkit matches its actions on the program path. Absent
	// systemctl means no systemd, which means no service to start — the
	// handler says so instead of guessing (#315/#317).
	{"internal/gui/tray/actions_linux.go", "systemctl", systemTool},
	{"internal/platform/service/service_linux.go", "useradd", systemTool},
	{"internal/platform/service/service_linux.go", "getent", systemTool},
	{"internal/platform/service/service_linux.go", "chown", systemTool},
	{"internal/proxy/trust/install_linux.go", "update-ca-certificates", systemTool},
	{"internal/platform/trayhost/repair_linux.go", "apt-get", systemTool},
	{"internal/proxy/trust/install_windows.go", "certutil", systemTool},

	// GPU vendor tools.
	//
	// This block used to read "absence is the answer, not a lookup
	// failure", and that reason was wrong in the same way the engine
	// predicates were: a driver tool missing from a LocalSystem service's
	// PATH says nothing about the driver. #67 was the cost — a host with a
	// working card profiled as CPU-only, silently. detectNvidia now
	// resolves through a chain ($WAIRED_NVIDIA_SMI → PATH → the OS's
	// well-known locations) and falls back to NVML / the OS device
	// inventory, so the site below is a hint inside that chain. The
	// declaration stays to keep the chain readable in one list, not to
	// bless a PATH-only verdict.
	{"internal/hardware/gpu_nvidia.go", "nvidia-smi", vendorTool},
	{"internal/hardware/gpu_amd.go", "rocm-smi", vendorTool},
	// vllm.go is Linux-only (the wheels are), where nvidia-smi ships in
	// /usr/bin and the vLLM install runs elevated with a normal PATH; it
	// also wants the tool itself for --query-compute-apps, not a presence
	// verdict. internal/runtime deliberately does not import
	// internal/hardware (see ollama_backend.go), so it keeps its own probe.
	{"internal/runtime/vllm.go", "nvidia-smi", vendorTool},

	// Third-party CLIs the user brings.
	{"internal/download/hf.go", "hf", userTool},
	{"internal/download/hf.go", "huggingface-cli", userTool},
	{"internal/runtime/uv.go", "uv", userTool},
	{"internal/integration/detect.go", "binary",
		userTool + " (the coding-agent CLIs: claude, opencode, code, …)"},

	// The one engine-adjacent site left. ollama.go is step 3 of a
	// documented chain — $WAIRED_OLLAMA_BINARY, then PATH, then the
	// well-known install locations — so PATH is a hint here, never the
	// verdict.
	//
	// internal/hardware/profiler.go used to sit beside it, frozen rather
	// than forgiven: defaultEngineVersion answered "which engine version
	// can I run" from PATH alone. #238 removed it — the daemon now hands
	// the profiler a resolved path (engineVersionOnHost →
	// hardware.EngineVersionAt), so no PATH probe is left to declare.
	{"internal/download/ollama.go", "ollamaCmdName",
		"step 3 of ResolveBinary's documented chain ($WAIRED_OLLAMA_BINARY → PATH → " +
			"well-known paths); PATH is a hint, not the verdict"},
}
