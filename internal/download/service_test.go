package download

import "testing"

// The origin a manual download records must be a value the schema accepts.
//
// It said "manual", which is not one of api, cli, auto_download or schedule -
// so every download run by hand failed at the insert with a CHECK constraint
// violation, and nothing in the API surfaced why. Manual-versus-automatic is
// carried by Trigger, which is what made the wrong value here look plausible.
func TestManualDownloadRecordsAnAcceptedOrigin(t *testing.T) {
	accepted := map[string]bool{
		"api": true, "cli": true, "auto_download": true, "schedule": true,
	}
	if !accepted[manualOrigin] {
		t.Fatalf("manual download origin = %q, which the request_origin CHECK "+
			"constraint rejects; accepted values are api, cli, auto_download "+
			"and schedule", manualOrigin)
	}
}
