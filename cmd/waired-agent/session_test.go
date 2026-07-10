package main

import (
	"context"
	"errors"
	"testing"

	"github.com/waired-ai/waired-agent/internal/identity"
	"github.com/waired-ai/waired-agent/internal/router"
)

// A nil (unpublished) session is the resting state of a fresh,
// unenrolled daemon. Read-only providers must return clean zero views
// (so the tray renders "not signed in") and action providers must
// return errNotEnrolled rather than panicking on the nil session.
func TestSwitchboardUnenrolledViews(t *testing.T) {
	sb := &switchboard{}
	ctx := context.Background()

	if got := sb.Status(); got.DeviceID != "" || got.PeerCount != 0 {
		t.Errorf("unenrolled Status should be zero, got %+v", got)
	}
	if got := sb.Identity(); got.Enrolled {
		t.Errorf("unenrolled Identity should have Enrolled=false, got %+v", got)
	}
	if _, err := sb.PingPeer(ctx, "peer"); !errors.Is(err, errNotEnrolled) {
		t.Errorf("PingPeer: want errNotEnrolled, got %v", err)
	}
	if err := sb.Pause(ctx); !errors.Is(err, errNotEnrolled) {
		t.Errorf("Pause: want errNotEnrolled, got %v", err)
	}
	if err := sb.Resume(ctx); !errors.Is(err, errNotEnrolled) {
		t.Errorf("Resume: want errNotEnrolled, got %v", err)
	}
	if cur, des := sb.Phase(); cur != "" || des != "" {
		t.Errorf("Phase: want empty, got (%q,%q)", cur, des)
	}

	infCtl := sbInfControl{sb}
	if err := infCtl.Enable(ctx); !errors.Is(err, errNotEnrolled) {
		t.Errorf("inf Enable: want errNotEnrolled, got %v", err)
	}
	if cur, des := infCtl.State(); cur != "" || des != "" {
		t.Errorf("inf State: want empty, got (%q,%q)", cur, des)
	}

	shareCtl := sbShareControl{sb}
	if err := shareCtl.Share(ctx); !errors.Is(err, errNotEnrolled) {
		t.Errorf("share Share: want errNotEnrolled, got %v", err)
	}

	workerCtl := sbWorkerControl{sb}
	if err := workerCtl.Clear(ctx); !errors.Is(err, errNotEnrolled) {
		t.Errorf("worker Clear: want errNotEnrolled, got %v", err)
	}
	if cur, des := workerCtl.State(); !cur.IsZero() || !des.IsZero() {
		t.Errorf("worker State: want zero RoutingPreference, got (%+v,%+v)", cur, des)
	}

	infProv := sbInfProvider{sb}
	if _, err := infProv.PullModel(ctx, "m"); !errors.Is(err, errNotEnrolled) {
		t.Errorf("inf PullModel: want errNotEnrolled, got %v", err)
	}
	if _, err := infProv.Select(ctx, router.Request{}); !errors.Is(err, errNotEnrolled) {
		t.Errorf("inf Select: want errNotEnrolled, got %v", err)
	}
	if got := infProv.Status(ctx); got.SubsystemState != "" {
		t.Errorf("inf Status: want zero, got %+v", got)
	}
}

// Once a session is published, the read-only providers delegate to the
// live session's concrete providers.
func TestSwitchboardDelegatesToLiveSession(t *testing.T) {
	sb := &switchboard{}
	prov := &agentProvider{
		id: &identity.Identity{
			DeviceID:     "dev-1",
			NetworkID:    "net-1",
			AccountEmail: "user@example.com",
			OverlayIP:    "10.10.0.2",
			ControlURL:   "https://cp.example",
		},
		wgListenPort: 51820,
	}
	s := &session{provider: prov}

	if !sb.publish(s) {
		t.Fatal("first publish should succeed")
	}
	if got := sb.Status(); got.DeviceID != "dev-1" || got.ListenPort != 51820 {
		t.Errorf("live Status not delegated: %+v", got)
	}
	id := sb.Identity()
	if !id.Enrolled || id.AccountEmail != "user@example.com" {
		t.Errorf("live Identity not delegated: %+v", id)
	}

	// CAS guard: a second publish over an already-live session is
	// rejected, so a boot/login race cannot clobber the current session.
	if sb.publish(&session{provider: prov}) {
		t.Error("second publish should be rejected by CAS")
	}
}
