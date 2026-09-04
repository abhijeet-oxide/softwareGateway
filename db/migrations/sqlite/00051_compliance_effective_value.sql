-- What actually applies, and what a finding stands down for.
--
-- effective_value is the other half of nearly every finding worth writing. A
-- report that says "allowPrivilegeEscalation is not declared" asks the reader
-- to know that Kubernetes fills that blank with "allowed"; one that also says
-- what applies in practice does not. The same gap is behind a percentage that
-- rounds to zero copies, a limit silently copied into the request, and a value
-- inherited from the pod rather than set on the container.
--
-- superseded_by names the check that owns a subject's root cause on a result
-- recorded as a skip. A privileged container was producing three blocking
-- findings, two of which could not be acted on at all: the kernel grants the
-- full capability set whatever capabilities.drop says. Those two are recorded
-- here rather than dropped, because a missing row and a passing row look the
-- same to a reader and neither is true.

-- +goose Up

ALTER TABLE compliance_results ADD COLUMN effective_value TEXT NOT NULL DEFAULT '';
ALTER TABLE compliance_results ADD COLUMN superseded_by   TEXT NOT NULL DEFAULT '';

-- +goose Down

ALTER TABLE compliance_results DROP COLUMN effective_value;
ALTER TABLE compliance_results DROP COLUMN superseded_by;
