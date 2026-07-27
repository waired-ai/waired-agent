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
	vendorTool = "GPU vendor driver tool; its presence IS the driver-installed signal"
	userTool   = "third-party CLI the user installed themselves; waired never manages it"
	ownCLI     = "waired's own CLI, which the installer puts on PATH because it is what " +
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
	{"cmd/waired/init_tray_linux.go", "runuser", privHelper},
	{"cmd/waired/init_tray_linux.go", "sudo", privHelper},
	{"internal/platform/browser/browser_linux.go", "runuser", privHelper},
	{"internal/platform/browser/browser_linux.go", "sudo", privHelper},
	{"internal/gui/tray/actions_linux.go", "pkexec", privHelper},

	// Desktop helpers. `name` is lookPathOK's parameter (apt-get today);
	// `prog.binary` walks the zenity/kdialog candidate list.
	{"cmd/waired/init_tray_linux.go", "name", desktopHelper},
	{"internal/gui/tray/actions_linux.go", "zenity", desktopHelper},
	{"internal/gui/tray/actions_linux.go", "kdialog", desktopHelper},
	{"internal/gui/tray/dialog_linux.go", "prog.binary", desktopHelper},
	{"internal/platform/notification/notification_linux.go", "notify-send", desktopHelper},

	// OS-provided system tools.
	{"internal/platform/service/service_linux.go", "systemctl", systemTool},
	{"internal/platform/service/service_linux.go", "useradd", systemTool},
	{"internal/platform/service/service_linux.go", "getent", systemTool},
	{"internal/platform/service/service_linux.go", "chown", systemTool},
	{"internal/proxy/trust/install_linux.go", "update-ca-certificates", systemTool},
	{"internal/proxy/trust/install_windows.go", "certutil", systemTool},

	// GPU vendor tools: absence is the answer, not a lookup failure.
	{"cmd/waired/setup_install.go", "nvidia-smi", vendorTool},
	{"internal/hardware/gpu_nvidia.go", "nvidia-smi", vendorTool},
	{"internal/hardware/gpu_amd.go", "rocm-smi", vendorTool},
	{"internal/runtime/vllm.go", "nvidia-smi", vendorTool},

	// Third-party CLIs the user brings.
	{"internal/download/hf.go", "hf", userTool},
	{"internal/download/hf.go", "huggingface-cli", userTool},
	{"internal/runtime/uv.go", "uv", userTool},
	{"internal/integration/detect.go", "binary",
		userTool + " (the coding-agent CLIs: claude, opencode, code, …)"},

	// The two engine-adjacent sites. Both are declared deliberately, and
	// they are not the same kind of thing.
	//
	// ollama.go is step 3 of a documented chain — $WAIRED_OLLAMA_BINARY,
	// then PATH, then the well-known install locations — so PATH is a
	// hint here, never the verdict.
	{"internal/download/ollama.go", "ollamaCmdName",
		"step 3 of ResolveBinary's documented chain ($WAIRED_OLLAMA_BINARY → PATH → " +
			"well-known paths); PATH is a hint, not the verdict"},
	//
	// profiler.go IS the #179 class, and is declared as such rather than
	// forgiven: defaultEngineVersion answers "which engine version can I
	// run" from PATH alone, so a bundled engine under the state dir
	// reports no version and the catalog picker sees no engine. It feeds
	// variant selection rather than the wizard's engine_installed, which
	// is why it did not produce the G1 chain — but it is the same
	// predicate, and the entry exists to freeze it, not to bless it.
	{"internal/hardware/profiler.go", "binary",
		"defaultEngineVersion's PATH-only engine probe — the #179 class, tracked by #238; " +
			"declared to freeze it, not to bless it"},
}
