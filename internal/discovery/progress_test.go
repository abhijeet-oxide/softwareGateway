package discovery

import "testing"

// The bar a scan draws must go up and only up.
//
// The complaint this exists for: "the progress bar keeps on changing, by going
// into hundred percent for repositories, then again for the versions". It did,
// because each phase was reported against its own denominator and the caller
// drew whichever was live. Three honest fractions in a row make one dishonest
// bar.
//
// So this walks a whole scan the way the scanner does — enumerate, list, check,
// fetch — and asserts the single number never falls.
func TestTheOverallBarOnlyEverMovesForward(t *testing.T) {
	var tracker progressTracker
	tracker.begin()

	last := tracker.snapshot().Overall
	step := func(what string, f func(*ScanProgress)) {
		tracker.update(f)
		now := tracker.snapshot().Overall
		if now < last {
			t.Errorf("%s took progress from %.3f back to %.3f — a bar that "+
				"retreats is reporting phases, not progress", what, last, now)
		}
		last = now
	}

	if last != 0 {
		t.Errorf("progress = %v before the catalog has come back, want 0: there is "+
			"no denominator yet and 0%% is what not knowing looks like", last)
	}

	step("listing begins", func(p *ScanProgress) {
		p.Phase = PhaseListingTags
		p.RepositoriesTotal = 4
	})
	for i := 1; i <= 4; i++ {
		step("a repository is listed", func(p *ScanProgress) {
			p.RepositoriesDone++
			// Listing is also what discovers the tags, so the next phase's
			// denominator arrives while this one is still running. This is
			// exactly what used to make the bar dip.
			p.TagsTotal += 25
		})
	}

	// Every repository listed, and the scan is a tenth of the way in — not all
	// the way, which is the whole complaint.
	if done := tracker.snapshot().Overall; done != listingShare {
		t.Errorf("with every repository listed progress = %v, want %v: listing is "+
			"the cheap phase and finishing it is not finishing the scan",
			done, listingShare)
	}

	step("resolving begins", func(p *ScanProgress) { p.Phase = PhaseResolving })
	for i := 0; i < 100; i++ {
		step("a tag is checked", func(p *ScanProgress) { p.TagsChecked++ })
	}

	// The reading pass adds its own work only once checking has decided how
	// much there is, so the denominator grows after the bar has already passed
	// most of it.
	step("the new tags are counted", func(p *ScanProgress) { p.TagsToFetch = 10 })
	for i := 0; i < 10; i++ {
		step("a new tag is read", func(p *ScanProgress) { p.TagsFetched++ })
	}

	if done := tracker.snapshot().Overall; done < 0.99 {
		t.Errorf("every tag checked and every new one read, progress = %v, want ~1", done)
	}
}

// Tags a Layout asks for on top of the ones the filters admitted are resolved
// too, so the checking pass can overshoot its own denominator. That must read
// as finished, not as 130%.
func TestAccessoryTagsCannotPushTheBarPastTheEnd(t *testing.T) {
	p := ScanProgress{
		Phase:       PhaseResolving,
		TagsTotal:   10,
		TagsChecked: 13,
	}
	if got := p.overall(); got != 1 {
		t.Errorf("overall = %v with more tags checked than listed, want 1", got)
	}
}
