package transfer

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/abhijeet-oxide/softwareGateway/internal/registry"
)

// What a refused tag write says about the tag it was refused.
//
// The three answers are the three things an operator can be told, and the third
// is as important as the other two: a note that might be wrong is worse than no
// note, so a destination that cannot answer produces none.

// tagStub answers the one question tagState asks and nothing else. The embedded
// interface is nil on purpose — a call to anything else is a bug in the code
// under test, and panicking says so.
type tagStub struct {
	registry.Repository
	desc registry.Descriptor
	err  error
}

func (s tagStub) ResolveTag(context.Context, string) (registry.Descriptor, error) {
	return s.desc, s.err
}

func TestARefusedTagIsReportedAsACreateOrAMove(t *testing.T) {
	const held = "sha256:1111111111111111111111111111111111111111111111111111111111111111"

	for _, tc := range []struct {
		name string
		stub tagStub
		want string
	}{
		{
			// Nothing there: the credential may not write tags at this path.
			name: "absent",
			stub: tagStub{err: registry.ErrNotFound},
			want: "would have created it",
		},
		{
			// Something else there: the credential may not MOVE this tag, which
			// is a different permission on the same path.
			name: "held by other content",
			stub: tagStub{desc: registry.Descriptor{Digest: registry.Digest(held)}},
			want: "would have moved it",
		},
		{
			// The destination would not say. Silence, rather than a guess
			// presented as a finding.
			name: "unanswerable",
			stub: tagStub{err: errors.New("connection reset")},
			want: "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := NewEngine(0, slog.New(slog.DiscardHandler))
			got := e.tagState(t.Context(), Job{Target: tc.stub}, "orb_23.8.1076")

			switch {
			case tc.want == "" && got != "":
				t.Errorf("said %q about a destination that gave no answer", got)
			case tc.want != "" && !strings.Contains(got, tc.want):
				t.Errorf("said %q, want it to state that the write %s", got, tc.want)
			}
			// The tag it names is the one that was refused, never a digest
			// standing in for it.
			if tc.name == "held by other content" && !strings.Contains(got, "sha256:111111111111") {
				t.Errorf("said %q without naming what the tag holds", got)
			}
		})
	}
}
