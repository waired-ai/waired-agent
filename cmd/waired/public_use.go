package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/spf13/cobra"

	"github.com/waired-ai/waired-agent/internal/agentconfig"
	"github.com/waired-ai/waired-agent/internal/management"
)

// newPublicUseCmd builds `waired public use` — the consumer-side settings
// verb. With no flags it prints the current settings (a viewer, no
// write). Any flag changes a setting and, on the very first use, walks the
// operator through the first-use privacy warning before anything is
// recorded (spec §4.2). All wording is plain English: no Waired-internal
// vocabulary.
func newPublicUseCmd() *cobra.Command {
	var mgmt string
	var jsonOut bool
	var auto, explicit, off bool
	var minTier int
	var mainStr, subStr string

	cmd := &cobra.Command{
		Use:   "use",
		Short: "Show or change whether this computer uses other people's public machines.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			upd, hasUpdate, err := buildPublicUseUpdate(cmd, auto, explicit, off, minTier, mainStr, subStr)
			if err != nil {
				return err
			}
			return runPublicUse(mgmt, upd, hasUpdate, jsonOut, cmd.OutOrStdout(), cmd.InOrStdin())
		},
	}

	cmd.Flags().BoolVar(&auto, "auto", false, "use public machines automatically when they beat your own")
	cmd.Flags().BoolVar(&explicit, "explicit", false, "use public machines only when a request is aimed at one")
	cmd.Flags().BoolVar(&off, "off", false, "never use other people's public machines")
	cmd.Flags().IntVar(&minTier, "min-tier", 0, "only use public machines at or above this quality tier")
	cmd.Flags().StringVar(&mainStr, "main", "", "on|off — let your main agent use public machines")
	cmd.Flags().StringVar(&subStr, "sub", "", "on|off — let sub-agents use public machines")
	addMgmtFlag(cmd, &mgmt)
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit the raw settings object as JSON")
	return cmd
}

// buildPublicUseUpdate translates the flags into a PublicUseUpdateRequest,
// setting ONLY the fields the operator actually passed (a nil pointer
// means "leave unchanged"). It also enforces the client-side rejections:
// the three mode flags are mutually exclusive, --min-tier may not be
// negative, and --main/--sub accept only on|off.
func buildPublicUseUpdate(cmd *cobra.Command, auto, explicit, off bool, minTier int, mainStr, subStr string) (management.PublicUseUpdateRequest, bool, error) {
	var upd management.PublicUseUpdateRequest
	hasUpdate := false

	modeCount := 0
	for _, set := range []bool{auto, explicit, off} {
		if set {
			modeCount++
		}
	}
	if modeCount > 1 {
		return upd, false, errors.New("waired public use: choose only one of --auto, --explicit, --off")
	}
	if modeCount == 1 {
		var mode string
		switch {
		case auto:
			mode = agentconfig.PublicUseModeAuto
		case explicit:
			mode = agentconfig.PublicUseModeExplicit
		case off:
			mode = agentconfig.PublicUseModeOff
		}
		upd.Mode = &mode
		hasUpdate = true
	}

	if cmd.Flags().Changed("min-tier") {
		if minTier < 0 {
			return upd, false, errors.New("waired public use: --min-tier must be >= 0")
		}
		v := minTier
		upd.MinQualityTier = &v
		hasUpdate = true
	}

	if cmd.Flags().Changed("main") {
		b, err := parseOnOff("--main", mainStr)
		if err != nil {
			return upd, false, err
		}
		upd.Main = &b
		hasUpdate = true
	}

	if cmd.Flags().Changed("sub") {
		b, err := parseOnOff("--sub", subStr)
		if err != nil {
			return upd, false, err
		}
		upd.Sub = &b
		hasUpdate = true
	}

	return upd, hasUpdate, nil
}

// parseOnOff maps the on|off string flags to a bool, rejecting anything
// else so a typo is surfaced rather than silently treated as off.
func parseOnOff(flagName, v string) (bool, error) {
	switch v {
	case "on":
		return true, nil
	case "off":
		return false, nil
	default:
		return false, fmt.Errorf("waired public use: %s must be on or off, got %q", flagName, v)
	}
}

// runPublicUse shows or updates the consumer-side Public Share settings.
// With no changes it GETs and prints. With a change it first ensures
// consent (a no-op once consented; a decline is a calm zero-exit "nothing
// changed"), then POSTs the update. A late consent_required 409 — the
// warning version bumped between consent and write — retries once.
func runPublicUse(mgmt string, upd management.PublicUseUpdateRequest, hasUpdate, jsonOut bool, out io.Writer, in io.Reader) error {
	if !hasUpdate {
		r, err := getPublicUse(mgmt)
		if err != nil {
			return err
		}
		return renderPublicUse(out, r, jsonOut)
	}

	if _, err := ensurePublicConsent(mgmt, out, in); err != nil {
		if errors.Is(err, errPublicConsentDeclined) {
			pln(out, "Nothing was changed.")
			return nil
		}
		return err
	}

	r, err := postPublicUse(mgmt, upd)
	if err != nil && mgmtErrorCode(err) == "consent_required" {
		// The daemon still wants consent (a warning-version bump raced the
		// write): run the flow once more and retry the update.
		if _, cerr := ensurePublicConsent(mgmt, out, in); cerr != nil {
			if errors.Is(cerr, errPublicConsentDeclined) {
				pln(out, "Nothing was changed.")
				return nil
			}
			return cerr
		}
		r, err = postPublicUse(mgmt, upd)
	}
	if err != nil {
		if isMgmtStatus(err, http.StatusNotFound) {
			return errPublicUseUnsupported
		}
		return fmt.Errorf("waired public use: %w", err)
	}
	return renderPublicUse(out, r, jsonOut)
}

// getPublicUse reads the current consumer settings, mapping a 404 to the
// upgrade-me sentinel.
func getPublicUse(mgmt string) (management.PublicUseResponse, error) {
	var r management.PublicUseResponse
	if err := publicGetJSON(mgmt, "/waired/v1/public/use", &r); err != nil {
		if isMgmtStatus(err, http.StatusNotFound) {
			return management.PublicUseResponse{}, errPublicUseUnsupported
		}
		return management.PublicUseResponse{}, fmt.Errorf("waired public use: %w", err)
	}
	return r, nil
}

func postPublicUse(mgmt string, upd management.PublicUseUpdateRequest) (management.PublicUseResponse, error) {
	var r management.PublicUseResponse
	err := publicPostJSON(mgmt, "/waired/v1/public/use", upd, &r)
	return r, err
}

func renderPublicUse(out io.Writer, r management.PublicUseResponse, jsonOut bool) error {
	if jsonOut {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(r)
	}
	printPublicUse(out, r)
	return nil
}

// printPublicUse renders the consumer-side settings block. It mirrors the
// consumer half of `waired public status` so the two read identically.
func printPublicUse(out io.Writer, r management.PublicUseResponse) {
	mode := r.EffectiveMode
	if mode == "" {
		mode = r.Mode
	}
	pf(out, "Use public nodes: %s\n", mode)
	pf(out, "Consented: %s\n", publicYesNo(r.Consented))
	pf(out, "Minimum quality tier: %d\n", r.MinQualityTier)
	pf(out, "Main agent: %s\n", publicOnOff(r.Main))
	pf(out, "Sub agents: %s\n", publicOnOff(r.Sub))
}
