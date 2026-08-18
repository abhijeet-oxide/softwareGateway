package store

import (
	"testing"
)

// seedReportFixture writes one product with two transfers: a succeeded copy
// that moved bytes and skipped some, and a failed one. Enough that every
// aggregate has something real to add up.
func seedReportFixture(t *testing.T) *Packages {
	t.Helper()
	s := openTestStore(t)
	p := NewPackages(s)
	db := s.DB()

	mustExec(t, db, `INSERT INTO products (id,name,config_hash,config) VALUES (1,'sbc','h','{}')`)
	mustExec(t, db, `INSERT INTO repositories (id,product_id,role,name,registry_host,repository_path,registry_type)
	                 VALUES (1,1,'source','nokia','vendor.example.com','sbc','generic'),
	                        (2,1,'target','jfrog','jfrog.example.com','sbc','generic')`)
	mustExec(t, db, `INSERT INTO packages (id,product_id,source_repo_id,tag,manifest_digest,media_type)
	                 VALUES (1,1,1,'25.8.1','sha256:aaa','application/vnd.oci.image.index.v1+json')`)
	mustExec(t, db, `INSERT INTO transfer_requests (id,product_id,package_id,operation,source_repo_id,idempotency_key)
	                 VALUES ('r1',1,1,'replicate',1,'k1'),
	                        ('r2',1,1,'promote',1,'k2'),
	                        ('r3',1,1,'replicate',1,'k3')`)

	// A copy that succeeded, with a measurable duration.
	mustExec(t, db, `INSERT INTO transfers
	    (id,request_id,package_id,source_repo_id,target_repo_id,state,strategy,
	     dedupe_skipped_bytes,started_at,completed_at)
	  VALUES ('t1','r1',1,1,2,'succeeded','copy',500,
	          '2026-08-10T10:00:00.000Z','2026-08-10T10:00:10.000Z')`)
	// A promotion that succeeded, and a transfer that failed.
	mustExec(t, db, `INSERT INTO transfers
	    (id,request_id,package_id,source_repo_id,target_repo_id,state,strategy,completed_at)
	  VALUES ('t2','r2',1,1,2,'succeeded','copy','2026-08-10T11:00:00.000Z'),
	         ('t3','r3',1,1,2,'failed','copy','2026-08-10T12:00:00.000Z')`)

	mustExec(t, db, `INSERT INTO jobs (transfer_id,kind,digest,size_bytes,source_repo_id,target_repo_id,state,bytes_transferred)
	                 VALUES ('t1','blob','sha256:b1',1000,1,2,'succeeded',1000),
	                        ('t1','blob','sha256:b2',3000,1,2,'skipped',0)`)
	mustExec(t, db, `INSERT INTO jobs (transfer_id,kind,digest,size_bytes,source_repo_id,target_repo_id,state,last_error_class)
	                 VALUES ('t3','blob','sha256:b3',10,2,2,'failed','upstream_unavailable')`)
	return p
}

func TestReportsCountsAndBytes(t *testing.T) {
	p := seedReportFixture(t)

	got, err := p.Reports(t.Context(), ReportFilter{})
	if err != nil {
		t.Fatalf("reports: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want one product, got %d", len(got))
	}
	r := got[0]

	if r.Product != "sbc" {
		t.Errorf("product = %q, want sbc", r.Product)
	}
	if r.Completed != 2 {
		t.Errorf("completed = %d, want 2", r.Completed)
	}
	if r.Failed != 1 {
		t.Errorf("failed = %d, want 1", r.Failed)
	}
	if r.Promotions != 1 {
		t.Errorf("promotions = %d, want 1", r.Promotions)
	}
	if r.BytesTransferred != 1000 {
		t.Errorf("bytesTransferred = %d, want 1000", r.BytesTransferred)
	}
	// 500 deduplicated at planning + 3000 skipped at run time.
	if r.SavedBytes != 3500 {
		t.Errorf("savedBytes = %d, want 3500", r.SavedBytes)
	}
	// Only t1 has both timestamps; t2 has no started_at and must not count,
	// or the speed denominator would include a transfer we never timed.
	if r.CopySeconds < 9.9 || r.CopySeconds > 10.1 {
		t.Errorf("copySeconds = %v, want ~10", r.CopySeconds)
	}
}

// The period bounds completion, and an empty bound means unbounded. Both
// spellings have to produce valid SQL, since periodBounds emits none, one or
// two predicates.
func TestReportsPeriodBounds(t *testing.T) {
	p := seedReportFixture(t)

	for _, tc := range []struct {
		name          string
		since, until  string
		wantCompleted int
	}{
		{"unbounded", "", "", 2},
		{"since only", "2026-08-10T10:30:00Z", "", 1},
		{"until only", "", "2026-08-10T10:30:00Z", 1},
		{"both", "2026-08-10T09:00:00Z", "2026-08-10T10:30:00Z", 1},
		{"outside", "2026-09-01T00:00:00Z", "", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := p.Reports(t.Context(), ReportFilter{Since: tc.since, Until: tc.until})
			if err != nil {
				t.Fatalf("reports: %v", err)
			}
			completed := 0
			if len(got) > 0 {
				completed = got[0].Completed
			}
			if completed != tc.wantCompleted {
				t.Errorf("completed = %d, want %d", completed, tc.wantCompleted)
			}
		})
	}
}

// The product filter is the authorization seam: a caller scoped to nothing
// this database knows about must get nothing back, not everything.
func TestReportsProductFilterScopes(t *testing.T) {
	p := seedReportFixture(t)

	got, err := p.Reports(t.Context(), ReportFilter{Products: []string{"bgw"}})
	if err != nil {
		t.Fatalf("reports: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("filtering to an unrelated product returned %d rows", len(got))
	}

	got, err = p.Reports(t.Context(), ReportFilter{Products: []string{"sbc", "bgw"}})
	if err != nil {
		t.Fatalf("reports: %v", err)
	}
	if len(got) != 1 || got[0].Product != "sbc" {
		t.Fatalf("want just sbc, got %+v", got)
	}
}

func TestFailureCausesGroupsByClass(t *testing.T) {
	p := seedReportFixture(t)

	got, err := p.FailureCauses(t.Context(), ReportFilter{})
	if err != nil {
		t.Fatalf("failure causes: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want one cause, got %+v", got)
	}
	if got[0].Class != "upstream_unavailable" || got[0].Jobs != 1 {
		t.Errorf("got %+v, want upstream_unavailable x1", got[0])
	}
}

func TestVolumeGroupsByDay(t *testing.T) {
	p := seedReportFixture(t)

	got, err := p.Volume(t.Context(), ReportFilter{})
	if err != nil {
		t.Fatalf("volume: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want one day, got %+v", got)
	}
	if got[0].Day != "2026-08-10" {
		t.Errorf("day = %q, want 2026-08-10", got[0].Day)
	}
	if got[0].BytesTransferred != 1000 {
		t.Errorf("bytes = %d, want 1000", got[0].BytesTransferred)
	}
}
