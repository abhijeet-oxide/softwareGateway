package api

import (
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/abhijeet-oxide/softwareGateway/internal/store"
	v1 "github.com/abhijeet-oxide/softwareGateway/pkg/apis/softwaregateway/v1"
)

// GET /api/v1/reports/summary - operational figures for a period.
//
// # Per product, always
//
// The rollup is computed per product and the estate total is the sum, rather
// than the other way round. That is not a presentation choice: a caller who
// may see two of five products must get those two added up, and a design that
// computed the estate total in SQL could not produce that without a second
// query written later, under pressure, by someone who has forgotten why.
//
// # What it will not state
//
// A speed it did not measure. AverageBytesPerSecond is derived from `copy`
// transfers alone - the ones whose bytes our workers moved and counted - and
// is OMITTED rather than zeroed when the period contains none
// (docs/design/18 §6.1). There is no verification metric at all, because
// nothing writes verification records yet.

func (s *Server) handleReportSummary(w http.ResponseWriter, r *http.Request) {
	if s.deps.Packages == nil {
		Error(w, r, v1.CodeUnavailable, "package storage is not configured")
		return
	}

	q := r.URL.Query()
	period, err := parsePeriod(q.Get("period"), q.Get("since"), q.Get("until"))
	if err != nil {
		Error(w, r, v1.CodeInvalidArgument, err.Error())
		return
	}

	products, visible := auditProducts(r, q.Get("product"))
	if !visible {
		WriteJSON(w, r, http.StatusOK, v1.ReportSummary{
			Period:   period,
			Products: []v1.ProductReport{},
		})
		return
	}

	filter := store.ReportFilter{Products: products, Since: period.Since, Until: period.Until}

	rows, err := s.deps.Packages.Reports(r.Context(), filter)
	if err != nil {
		s.internal(w, r, "report summary", err)
		return
	}
	causes, err := s.deps.Packages.FailureCauses(r.Context(), filter)
	if err != nil {
		s.internal(w, r, "report failure causes", err)
		return
	}
	volume, err := s.deps.Packages.Volume(r.Context(), filter)
	if err != nil {
		s.internal(w, r, "report volume", err)
		return
	}

	out := v1.ReportSummary{Period: period, Products: []v1.ProductReport{}}

	// The estate total is accumulated from the same rows the per-product
	// figures come from, so the two can never disagree.
	var estate store.ProductReport
	for _, row := range rows {
		out.Products = append(out.Products, v1.ProductReport{
			Product: row.Product,
			Totals:  reportTotals(row),
		})
		estate.Completed += row.Completed
		estate.Failed += row.Failed
		estate.Cancelled += row.Cancelled
		estate.Running += row.Running
		estate.Promotions += row.Promotions
		estate.BytesTransferred += row.BytesTransferred
		estate.SavedBytes += row.SavedBytes
		estate.CopyBytes += row.CopyBytes
		estate.CopySeconds += row.CopySeconds
	}
	out.Totals = reportTotals(estate)

	for _, c := range causes {
		class := c.Class
		if class == "" {
			class = "unclassified"
		}
		out.FailureCauses = append(out.FailureCauses,
			v1.FailureCause{Product: c.Product, Class: class, Jobs: c.Jobs})
	}
	for _, v := range volume {
		out.Volume = append(out.Volume, v1.DailyVolume{
			Day:              v.Day,
			BytesTransferred: v1.Int64String(strconv.FormatInt(v.BytesTransferred, 10)),
			Downloads:        v.Transfers,
		})
	}

	WriteJSON(w, r, http.StatusOK, out)
}

func reportTotals(r store.ProductReport) v1.ReportTotals {
	t := v1.ReportTotals{
		DownloadsCompleted: r.Completed,
		DownloadsFailed:    r.Failed,
		DownloadsCancelled: r.Cancelled,
		DownloadsRunning:   r.Running,
		Promotions:         r.Promotions,
		BytesTransferred:   v1.Int64String(strconv.FormatInt(r.BytesTransferred, 10)),
		SavedBytes:         v1.Int64String(strconv.FormatInt(r.SavedBytes, 10)),
	}

	// A speed needs both a byte count we took and a duration we timed. With
	// neither, or with a duration of zero, there is no honest answer and the
	// field stays absent rather than becoming zero or infinity.
	if r.CopySeconds > 0 && r.CopyBytes > 0 {
		// Rounded, not truncated. The duration arrives as a float - SQLite
		// computes it through julianday, which lands a ten-second transfer on
		// 10.0000001 - and truncating 99.999 to 99 would put a number on
		// screen that is wrong in the last digit for no reason anyone could
		// explain from the data.
		bps := math.Round(float64(r.CopyBytes) / r.CopySeconds)
		speed := v1.Int64String(strconv.FormatInt(int64(bps), 10))
		t.AverageBytesPerSecond = &speed
	}

	// Saved against everything that was in play. Absent when nothing was:
	// "0% saved" and "nothing happened" are different claims.
	if total := r.BytesTransferred + r.SavedBytes; total > 0 {
		pct := float64(r.SavedBytes) / float64(total) * 100
		t.SavedPercent = &pct
	}
	if settled := r.Completed + r.Failed; settled > 0 {
		rate := float64(r.Completed) / float64(settled)
		t.SuccessRate = &rate
	}
	return t
}

// parsePeriod accepts either a shorthand label or an explicit range.
//
// The response echoes back what was applied, so a figure is never shown
// against a period the reader assumed rather than one the server used.
func parsePeriod(label, since, until string) (v1.ReportPeriod, error) {
	if label != "" && (since != "" || until != "") {
		return v1.ReportPeriod{}, fmt.Errorf(
			"period and since/until name the same window two ways; use one")
	}

	if label == "" && since == "" && until == "" {
		label = "30d"
	}

	if label != "" {
		days, err := parseDayLabel(label)
		if err != nil {
			return v1.ReportPeriod{}, err
		}
		start := time.Now().UTC().Add(-time.Duration(days) * 24 * time.Hour)
		return v1.ReportPeriod{
			Since: start.Format(time.RFC3339Nano),
			Label: label,
		}, nil
	}

	out := v1.ReportPeriod{}
	var err error
	if out.Since, err = parseTimeBound(since, "since"); err != nil {
		return v1.ReportPeriod{}, err
	}
	if out.Until, err = parseTimeBound(until, "until"); err != nil {
		return v1.ReportPeriod{}, err
	}
	return out, nil
}

func parseDayLabel(label string) (int, error) {
	digits, ok := strings.CutSuffix(label, "d")
	if !ok {
		return 0, fmt.Errorf("period must be a number of days such as 7d or 30d, got %q", label)
	}
	days, err := strconv.Atoi(digits)
	if err != nil || days <= 0 || days > 3650 {
		return 0, fmt.Errorf("period must be between 1d and 3650d, got %q", label)
	}
	return days, nil
}
