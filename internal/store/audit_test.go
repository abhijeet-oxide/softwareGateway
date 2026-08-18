package store

import "testing"

func seedAudit(t *testing.T) *Packages {
	t.Helper()
	s := openTestStore(t)
	p := NewPackages(s)
	db := s.DB()

	mustExec(t, db, `INSERT INTO audit_events
	    (occurred_at,event_type,actor,actor_kind,product_name,subject_kind,subject_id,outcome,detail)
	  VALUES ('2026-08-10T10:00:00.000Z','PackageDiscovered','system','system','sbc','package','25.8.1','success','{"tag":"25.8.1"}'),
	         ('2026-08-10T11:00:00.000Z','DownloadRunRequested','ab','user','sbc','package','25.8.1','success',NULL),
	         ('2026-08-10T12:00:00.000Z','PackageDiscovered','system','system','bgw','package','25.7.4','failure',NULL)`)
	return p
}

func TestListAuditNewestFirst(t *testing.T) {
	p := seedAudit(t)

	got, err := p.ListAudit(t.Context(), AuditFilter{})
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("want 3 events, got %d", len(got))
	}
	if got[0].ProductName != "bgw" {
		t.Errorf("newest event is %q, want the bgw one", got[0].ProductName)
	}
	// A NULL detail must read as empty rather than failing the scan: most
	// producers write none.
	if got[1].Detail != "" {
		t.Errorf("detail = %q, want empty for a NULL", got[1].Detail)
	}
	if got[2].Detail == "" {
		t.Errorf("detail was dropped for the row that has one")
	}
}

// Products is the authorization seam, so it has to actually narrow.
func TestListAuditScopesToProducts(t *testing.T) {
	p := seedAudit(t)

	got, err := p.ListAudit(t.Context(), AuditFilter{Products: []string{"sbc"}})
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 sbc events, got %d", len(got))
	}
	for _, e := range got {
		if e.ProductName != "sbc" {
			t.Errorf("leaked a %q event through an sbc scope", e.ProductName)
		}
	}
}

func TestListAuditFilters(t *testing.T) {
	p := seedAudit(t)

	for _, tc := range []struct {
		name string
		f    AuditFilter
		want int
	}{
		{"by event type", AuditFilter{EventType: "PackageDiscovered"}, 2},
		{"by actor", AuditFilter{Actor: "ab"}, 1},
		{"by outcome", AuditFilter{Outcome: "failure"}, 1},
		{"by subject", AuditFilter{SubjectKind: "package", SubjectID: "25.8.1"}, 2},
		{"since", AuditFilter{Since: "2026-08-10T10:30:00Z"}, 2},
		{"until", AuditFilter{Until: "2026-08-10T10:30:00Z"}, 1},
		{"composed", AuditFilter{Products: []string{"sbc"}, Outcome: "success", Actor: "ab"}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := p.ListAudit(t.Context(), tc.f)
			if err != nil {
				t.Fatalf("list audit: %v", err)
			}
			if len(got) != tc.want {
				t.Errorf("got %d events, want %d", len(got), tc.want)
			}
		})
	}
}

func TestListAuditPaginates(t *testing.T) {
	p := seedAudit(t)

	first, err := p.ListAudit(t.Context(), AuditFilter{Limit: 2})
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}
	second, err := p.ListAudit(t.Context(), AuditFilter{Limit: 2, Offset: 2})
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}
	if len(first) != 2 || len(second) != 1 {
		t.Fatalf("pages were %d and %d, want 2 and 1", len(first), len(second))
	}
	if first[0].ID == second[0].ID {
		t.Error("the second page repeated a row from the first")
	}
}
