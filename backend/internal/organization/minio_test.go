//go:build linux || darwin

package organization

import (
	"strings"
	"testing"
	"time"
)

func TestMinioModesAndGrantsNeverBorrowRcloneAuthority(t *testing.T) {
	d, _, _, _, _ := remoteUnitFixture(t)
	if RemoteTargetIdentityFor("rclonecopy", d.TargetRoot) != RemoteTargetIdentity(d.TargetRoot) {
		t.Fatal("old Rclone target identity changed")
	}
	d.Mode = "miniocopy"
	d.Remote.Driver = "minio"
	d.TargetID = "configured-root"
	d.TargetIdentity = RemoteTargetIdentityFor(d.Mode, d.TargetRoot)
	if !SupportedMode(d.Mode) || !RemoteMode(d.Mode) || MoveMode(d.Mode) || !MoveMode("minio") || ModeLabel("minio") != "Minio移动" || !remoteProfileValid(d) {
		t.Fatal(d)
	}
	g := &RemoteAutomation{Protocol: 1, Profiles: map[string]RemoteProfileSeal{d.TargetID: {Digest: Digest(d.Remote)}}, Exclusive: true, RecoveryCopy: true, ApprovedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	if g.Allows(d) {
		t.Fatal("old Rclone key granted MinIO")
	}
	g.Profiles = map[string]RemoteProfileSeal{RemoteProfileKey(d.TargetID, "minio"): {Driver: "minio", Digest: Digest(d.Remote), Directories: d.Remote.Directories}}
	if !g.Allows(d) {
		t.Fatal("explicit MinIO grant not usable")
	}
	d.Mode = "rclonecopy"
	if g.Allows(d) || remoteProfileValid(d) {
		t.Fatal("mode changed provider without new review")
	}
	if RemoteTargetIdentityFor("minio", d.TargetRoot) == RemoteTargetIdentity(d.TargetRoot) || len(strings.TrimSpace(d.TargetIdentity)) != 64 {
		t.Fatal("provider identities collided")
	}
}
