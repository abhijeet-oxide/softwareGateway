package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/abhijeet-oxide/softwareGateway/internal/preflight"
	"github.com/abhijeet-oxide/softwareGateway/internal/product"
	v1 "github.com/abhijeet-oxide/softwareGateway/pkg/apis/softwaregateway/v1"
)

// handleCheckConnectivity serves
// POST /api/v1/products:checkConnectivity and
// POST /api/v1/products/{product}:checkConnectivity.
//
// An AIP-136 custom method — a verb with side effects (outbound network calls)
// rather than a resource.
//
// Deliberately NOT folded into the health check. Health answers "is the service
// working?" and its machinery backs readiness; making it depend on third-party
// registries would mean a vendor's outage pulls our pods out of service, and
// would leave an operator unable to tell whose fault an unhealthy reading was.
// See docs/design/09 §9.1.
func (s *Server) handleCheckConnectivity(w http.ResponseWriter, r *http.Request) {
	if s.deps.Products == nil || s.deps.Preflight == nil {
		Error(w, r, v1.CodeUnavailable, "connectivity checking is not configured")
		return
	}

	var targets []*product.Product

	if name := chi.URLParam(r, "product"); name != "" {
		p, ok := s.deps.Products.Get(name)
		if !ok {
			NotFound(w, r, "product", name)
			return
		}
		targets = []*product.Product{p}
	} else {
		targets = s.deps.Products.List()
	}

	resp := v1.CheckConnectivityResponse{Status: v1.CheckOK, Products: []v1.ProductCheck{}}

	for _, p := range targets {
		res := s.deps.Preflight.CheckProduct(r.Context(), p)
		resp.Products = append(resp.Products, toAPIProductCheck(res))

		switch res.Status {
		case preflight.StatusFailed:
			resp.Status = v1.CheckFailed
		case preflight.StatusWarning:
			if resp.Status != v1.CheckFailed {
				resp.Status = v1.CheckWarning
			}
		case preflight.StatusOK, preflight.StatusSkipped:
		}
	}

	// Always 200. This is a diagnostic report and its BODY carries the verdict;
	// returning 503 would make a CLI that checks status codes unable to show
	// WHICH repository is unhappy, which is the only useful part.
	WriteJSON(w, r, http.StatusOK, resp)
}

func toAPIProductCheck(res preflight.ProductResult) v1.ProductCheck {
	out := v1.ProductCheck{
		Product:      res.Product,
		Status:       v1.CheckStatus(res.Status),
		Repositories: []v1.RepositoryCheck{},
	}
	for _, r := range res.Repositories {
		rc := v1.RepositoryCheck{
			Name:       r.Name,
			Role:       r.Role,
			Registry:   r.Registry,
			Repository: r.Repository,
			Status:     v1.CheckStatus(r.Status),
			Steps:      []v1.CheckStep{},
		}
		for _, st := range r.Steps {
			rc.Steps = append(rc.Steps, v1.CheckStep{
				Name:      st.Name,
				Status:    v1.CheckStatus(st.Status),
				Detail:    st.Detail,
				Hint:      st.Hint,
				LatencyMs: float64(st.Latency.Microseconds()) / 1000,
			})
		}
		out.Repositories = append(out.Repositories, rc)
	}
	return out
}
