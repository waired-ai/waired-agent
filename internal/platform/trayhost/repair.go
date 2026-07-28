package trayhost

// Repair is the second half of this package: Check says whether the tray icon
// will render, and the code here says what — if anything — can be done about a
// NoHost verdict, and how much privilege it costs.
//
// The split matters because the two halves of the fix live in different
// privilege domains (#295):
//
//   - Installing the AppIndicator host extension needs root (apt).
//   - Enabling it needs the user's own GNOME session, and no privilege at all —
//     `gnome-extensions enable` writes one string into the invoking user's
//     dconf.
//
// So a host whose extension is merely disabled is repairable for free by
// waired-tray itself at session start, while a host that is missing the package
// has to wait for install.sh (already root) or `waired doctor --fix` (sudo).

// RepairAction is what the caller should do about a Result.
type RepairAction int

const (
	// RepairNone means do nothing: the icon already renders, the question
	// does not apply here (non-Linux, headless), or nothing we could run
	// would help (MATE cannot draw SNI at all; a non-GNOME desktop without a
	// host is not ours to fix).
	RepairNone RepairAction = iota
	// RepairEnableOnly means an AppIndicator host extension is installed but
	// no SNI host is registered — it just is not enabled. Enable costs no
	// privilege, so the caller may simply do it.
	RepairEnableOnly
	// RepairInstallThenEnable means a GNOME host with no extension at all and
	// apt available: install the package, then enable it. Needs root.
	RepairInstallThenEnable
	// RepairManual means a GNOME host with no extension and no apt to install
	// one (Fedora, Arch, a stripped image). The operator has to install it
	// themselves; Result.Hint carries the wording.
	RepairManual
)

func (a RepairAction) String() string {
	switch a {
	case RepairEnableOnly:
		return "enable-only"
	case RepairInstallThenEnable:
		return "install-then-enable"
	case RepairManual:
		return "manual"
	default:
		return "none"
	}
}

// Fixable reports whether waired can carry the repair out itself. False for
// RepairManual: there is advice to print, but no command of ours to run.
func (a RepairAction) Fixable() bool {
	return a == RepairEnableOnly || a == RepairInstallThenEnable
}

// NeedsPrivilege reports whether carrying the action out requires root. Only
// the apt step does; enabling an extension is a per-user dconf write.
func (a RepairAction) NeedsPrivilege() bool { return a == RepairInstallThenEnable }

// RepairFacts are the inputs PlanRepair acts on. Status and Desktop come
// straight from Check; the rest are host probes (see GatherRepairFacts).
type RepairFacts struct {
	// Status is Check().Status.
	Status Status
	// Desktop is Check().Desktop.
	Desktop Desktop
	// ExtensionPresent is true when a known AppIndicator host extension
	// directory exists, system-wide or for this user.
	ExtensionPresent bool
	// GnomeShellOnPath is true when gnome-shell is installed on this host.
	// This is the server guard — see PlanRepair.
	GnomeShellOnPath bool
	// AptOnPath is true when apt-get is available to install with.
	AptOnPath bool
}

// PlanRepair is the pure decision behind every caller (doctor, the tray, and
// the reasoning install.sh mirrors in shell). goos is a parameter rather than
// runtime.GOOS so the whole matrix is table-tested on every platform.
//
// The load-bearing rule is the GnomeShellOnPath gate. `apt install
// gnome-shell-extension-appindicator` looks harmless, but on Ubuntu 26.04 that
// name is a *virtual* package whose only provider is gnome-shell-ubuntu-extensions,
// which itself `Depends: gnome-shell (>= 49~)`. Running it on a server would
// therefore install GNOME Shell, gjs and the whole gir stack onto a headless
// box. Requiring gnome-shell to already be present makes that impossible: apt
// can only ever pull the extension itself. This is also why the package
// relationship in packaging/nfpm/waired-tray.yaml.tmpl stays `suggests` and is
// never promoted to `recommends` — apt has no conditional-dependency form, so
// the decision has to be made at runtime, here.
func PlanRepair(goos string, f RepairFacts) RepairAction {
	if goos != "linux" {
		// macOS and Windows have native tray hosts; there is nothing to
		// install or enable. Check already returns NotApplicable there, but
		// the guard keeps this total for any caller that builds facts itself.
		return RepairNone
	}
	if f.Status != NoHost {
		// HostPresent → the icon renders. NotApplicable → headless. Unsupported
		// → MATE, which cannot render SNI at all, so no package would help.
		return RepairNone
	}
	if !f.GnomeShellOnPath {
		// Not a GNOME host. Either the desktop ships its own host (KDE) and
		// something else is wrong, or it is one we have no extension for.
		// Never install: see the doc comment.
		return RepairNone
	}
	if f.ExtensionPresent {
		return RepairEnableOnly
	}
	if !f.AptOnPath {
		return RepairManual
	}
	return RepairInstallThenEnable
}
