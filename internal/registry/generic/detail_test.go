package generic

import "testing"

// What the registry said, when it did not say it the way the specification
// expects.
//
// A registry in front of an entitlement system answers a request for an
// unlicensed product with a sentence naming the customer, the product and the
// reason. It is not the Distribution error shape, so it parsed to nothing and
// the operator was shown `HTTP 403: forbidden` — a status code where there was
// a diagnosis. Thirty-seven of those look like a broken credential.

func TestTheRegistrysOwnSentenceSurvivesANonStandardErrorBody(t *testing.T) {
	body := []byte(`{
	  "status": 403,
	  "ErrorType": "User Authorization",
	  "ErrorMessage": "No valid entitlement found for End User: 215952 and Product sales items: CFXC24STD03.00.",
	  "Action": "Please contact customer team to make sure the customer is entitled to the product release you are trying to download.",
	  "LogTag": "wn7E1Z_wZ"
	}`)

	got := vendorErrorDetail(body)
	want := "No valid entitlement found for End User: 215952 and Product sales items: CFXC24STD03.00."
	if got != want {
		t.Errorf("detail = %q, want %q", got, want)
	}
}

func TestOtherCommonErrorBodiesAreReadToo(t *testing.T) {
	for _, tc := range []struct {
		name, body, want string
	}{
		{"message", `{"message":"repository not accessible"}`, "repository not accessible"},
		{"error", `{"error":"quota exceeded"}`, "quota exceeded"},
		{"detail", `{"detail":"blocked by policy"}`, "blocked by policy"},
		// A type with no message still says which system refused.
		{"type only", `{"ErrorType":"User Authorization"}`, "User Authorization"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := vendorErrorDetail([]byte(tc.body)); got != tc.want {
				t.Errorf("detail = %q, want %q", got, tc.want)
			}
		})
	}
}

// Anything unrecognised yields nothing, which is the behaviour this replaced.
// It may only ever ADD detail — inventing one from an arbitrary body would put
// a registry's HTML error page into an operator's terminal.
func TestAnUnrecognisedBodyAddsNothing(t *testing.T) {
	for _, body := range []string{
		`<html><body>403 Forbidden</body></html>`,
		`{"unrelated":"field"}`,
		``,
	} {
		if got := vendorErrorDetail([]byte(body)); got != "" {
			t.Errorf("detail = %q for %q, want empty", got, body)
		}
	}
}
