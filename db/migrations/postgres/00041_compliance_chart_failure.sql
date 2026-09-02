-- Why a chart did not render, and how hard it was tried.
--
-- "helm template failed" was one sentence over a coverage table of ninety-five
-- charts, seventeen of which failed for four different reasons: a subchart that
-- requires `global.registry` and was rendered without an umbrella to supply it,
-- a values.schema.json the vendor's own defaults violate, a template
-- dereferencing a nil, and a file that is not valid YAML.
--
-- Those are four different conversations - three with the vendor and one with
-- us - and an undifferentiated list of stack traces is how they all become "the
-- tool is broken". The classification is what lets the coverage table group and
-- count them, and what lets the run say which failures a retry could not have
-- fixed. See internal/compliance/render/failure.go.

-- +goose Up

ALTER TABLE compliance_charts
    ADD COLUMN error_kind TEXT NOT NULL DEFAULT '';

-- How many renders were attempted. Recorded because "retried and failed again"
-- and "not retried, because a second render of the same bytes returns the same
-- error" are different facts, and a reader is entitled to know which happened.
ALTER TABLE compliance_charts
    ADD COLUMN attempts INTEGER NOT NULL DEFAULT 0;

-- +goose Down

ALTER TABLE compliance_charts DROP COLUMN error_kind;
ALTER TABLE compliance_charts DROP COLUMN attempts;
